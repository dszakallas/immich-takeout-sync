package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dszakallas/immich-takeout-sync/internal/config"
	"github.com/spf13/cobra"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/drive/v3"
)

var (
	clientID     string
	clientSecret string
	outputFile   string
	authPort     int
)

var authCmd = &cobra.Command{
	Use:   "auth",
	Short: "Google Drive authentication helpers",
}

type googleAppCredentials struct {
	Type         string `json:"type"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	Installed    *struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	} `json:"installed"`
	Web *struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	} `json:"web"`
}

func loadClientCredentials(source string) (string, string, error) {
	trimmed := strings.TrimSpace(source)
	var data []byte
	if strings.HasPrefix(trimmed, "{") {
		data = []byte(trimmed)
	} else {
		cleaned := filepath.Clean(source)
		// #nosec G304 -- path is user-specified or standard credential file path
		readData, err := os.ReadFile(cleaned)
		if err != nil {
			return "", "", err
		}
		data = readData
	}

	var creds googleAppCredentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return "", "", fmt.Errorf("parsing credentials: %w", err)
	}

	if creds.ClientID != "" && creds.ClientSecret != "" {
		return creds.ClientID, creds.ClientSecret, nil
	}
	if creds.Installed != nil && creds.Installed.ClientID != "" && creds.Installed.ClientSecret != "" {
		return creds.Installed.ClientID, creds.Installed.ClientSecret, nil
	}
	if creds.Web != nil && creds.Web.ClientID != "" && creds.Web.ClientSecret != "" {
		return creds.Web.ClientID, creds.Web.ClientSecret, nil
	}

	return creds.ClientID, creds.ClientSecret, nil
}

var loginCmd = &cobra.Command{
	Use:   "login",
	Short: "Interactive OAuth2 login to generate refresh token for personal Google Drive",
	Long: `Generates an OAuth2 token JSON file containing client_id, client_secret, and refresh_token.
Use this file in Kubernetes Secret (or with SOPS) to authenticate takeout-sync with personal Google Drive.`,
	RunE: func(_ *cobra.Command, _ []string) error {
		if clientID == "" || clientSecret == "" {
			candidateFiles := []string{
				credentialsJSON,
				credentialsFile,
				os.Getenv("GOOGLE_CREDENTIALS_JSON"),
				outputFile,
				os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"),
				config.DefaultCredentialsPath(),
				filepath.Join(config.XDGConfigHome(), "takeout-sync", config.DefaultCredentialsFileName),
				"google-oauth2-credentials.json",
				"creds.json",
			}
			for _, candidate := range candidateFiles {
				if candidate == "" {
					continue
				}
				id, secret, err := loadClientCredentials(candidate)
				if err == nil {
					if clientID == "" {
						clientID = id
					}
					if clientSecret == "" {
						clientSecret = secret
					}
					if clientID != "" && clientSecret != "" {
						break
					}
				}
			}
		}

		if clientID == "" || clientSecret == "" {
			return fmt.Errorf("--client-id and --client-secret are required (create a 'Desktop App' OAuth client in Google Cloud Console or provide a credentials file)")
		}

		redirectURI := fmt.Sprintf("http://localhost:%d/oauth/callback", authPort)
		conf := &oauth2.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			RedirectURL:  redirectURI,
			Scopes:       []string{drive.DriveScope},
			Endpoint:     google.Endpoint,
		}

		// Use offline access to receive a refresh_token
		authURL := conf.AuthCodeURL("state-token", oauth2.AccessTypeOffline, oauth2.ApprovalForce)

		fmt.Println("\n1. Open the following URL in your browser:")
		fmt.Println(authURL)
		fmt.Printf("\n2. Waiting for authentication on %s ...\n", redirectURI)

		codeChan := make(chan string, 1)
		errChan := make(chan error, 1)

		server := &http.Server{
			ReadHeaderTimeout: 10 * time.Second,
		}
		mux := http.NewServeMux()
		mux.HandleFunc("/oauth/callback", func(w http.ResponseWriter, r *http.Request) {
			code := r.URL.Query().Get("code")
			if code == "" {
				http.Error(w, "Code not found in callback", http.StatusBadRequest)
				errChan <- fmt.Errorf("no authorization code received in callback")
				return
			}
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<h2>Authentication successful! You can now close this tab.</h2>"))
			codeChan <- code
		})
		server.Handler = mux

		listener, err := net.Listen("tcp", fmt.Sprintf("localhost:%d", authPort))
		if err != nil {
			return fmt.Errorf("listening on port %d: %w", authPort, err)
		}

		go func() {
			if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
				errChan <- err
			}
		}()

		var code string
		select {
		case code = <-codeChan:
		case err := <-errChan:
			_ = server.Close()
			return err
		}

		_ = server.Shutdown(context.Background())

		token, err := conf.Exchange(context.Background(), code)
		if err != nil {
			return fmt.Errorf("exchanging code for token: %w", err)
		}

		if token.RefreshToken == "" {
			return fmt.Errorf("google did not return a refresh token: revoke access at https://myaccount.google.com/permissions and retry with approval_prompt=force")
		}

		output := map[string]string{
			"type":          "authorized_user",
			"client_id":     clientID,
			"client_secret": clientSecret,
			"refresh_token": token.RefreshToken,
		}

		data, err := json.MarshalIndent(output, "", "  ")
		if err != nil {
			return err
		}

		if outputFile != "" {
			cleanedOut := filepath.Clean(outputFile)
			dir := filepath.Dir(cleanedOut)
			if err := os.MkdirAll(dir, 0700); err != nil {
				return fmt.Errorf("creating directory %s: %w", dir, err)
			}
			// #nosec G306 -- credentials file contains secrets and must be restricted to 0600
			if err := os.WriteFile(cleanedOut, data, 0600); err != nil {
				return fmt.Errorf("writing output file: %w", err)
			}
			fmt.Printf("\nSuccessfully saved OAuth2 credentials to %s\n", cleanedOut)
		} else {
			fmt.Println("\nGenerated credentials JSON:")
			fmt.Println(string(data))
		}

		return nil
	},
}

func init() {
	loginCmd.Flags().StringVar(&clientID, "client-id", "", "Google OAuth2 Client ID")
	loginCmd.Flags().StringVar(&clientSecret, "client-secret", "", "Google OAuth2 Client Secret")
	loginCmd.Flags().StringVarP(&outputFile, "output", "o", config.DefaultCredentialsPath(), "Output file path for generated JSON credentials")
	loginCmd.Flags().IntVar(&authPort, "port", 8085, "Local HTTP server port for OAuth callback")

	authCmd.AddCommand(loginCmd)
}
