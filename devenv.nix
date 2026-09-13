{ pkgs, lib, config, ... }:

{
  languages.go.enable = true;

  packages = [
    pkgs.golangci-lint
    pkgs.immich-go
    pkgs.curl
    pkgs.jq
  ];

  scripts.immich-takeout-sync.exec = ''
    exec go run "$DEVENV_ROOT/main.go" "$@"
  '';
  scripts.takeout-sync.exec = ''
    exec immich-takeout-sync "$@"
  '';

  pre-commit.hooks = {
    gofmt.enable = true;
    govet.enable = true;
    golangci-lint.enable = true;
  };

  enterTest = ''
    go test -v -race ./...
  '';

  profiles.dev.module = {
    scripts.immich-bootstrap.exec = ''
      set -euo pipefail
      IMMICH_URL="http://127.0.0.1:''${toString config.processes.immich.ports.http.value}"
      echo "Waiting for Immich at $IMMICH_URL..."
      until curl -s -f "$IMMICH_URL/api/server/ping" > /dev/null 2>&1; do
        sleep 1
      done

      # Register initial admin user if not already registered
      curl -s -X POST "$IMMICH_URL/api/auth/admin-sign-up" \
        -H "Content-Type: application/json" \
        -d '{"email":"admin@example.com","password":"admin","name":"Admin"}' > /dev/null 2>&1 || true

      # Login as admin
      LOGIN_RES=$(curl -s -X POST "$IMMICH_URL/api/auth/login" \
        -H "Content-Type: application/json" \
        -d '{"email":"admin@example.com","password":"admin"}')
      TOKEN=$(echo "$LOGIN_RES" | jq -r '.accessToken // empty')

      if [ -n "$TOKEN" ]; then
        echo "Immich dev server ready at $IMMICH_URL (admin@example.com / admin)"
        KEY_RES=$(curl -s -X POST "$IMMICH_URL/api/api-keys" \
          -H "Authorization: Bearer $TOKEN" \
          -H "Content-Type: application/json" \
          -d '{"name":"dev-key","permissions":["all"]}')
        KEY=$(echo "$KEY_RES" | jq -r '.secret // empty')
        if [ -n "$KEY" ]; then
          echo "API key created: $KEY"
        fi
      fi
    '';

    # Auto-allocated dynamic ports
    processes.immich = {
      ports.http.allocate = 2283;

      exec = ''
        DATA_DIR="$DEVENV_ROOT/.data/immich"
        mkdir -p "$DATA_DIR"

        # Remove previous container if left running
        podman rm -f immich-dev 2>/dev/null || true

        exec podman run --rm --name immich-dev \
          -p ''${toString config.processes.immich.ports.http.value}:2283 \
          -e DB_HOSTNAME=host.containers.internal \
          -e DB_PORT=''${toString config.processes.postgres.ports.main.value} \
          -e DB_USERNAME=immich \
          -e DB_PASSWORD=immich \
          -e DB_DATABASE_NAME=immich \
          -e DB_VECTOR_EXTENSION=pgvector \
          -e REDIS_HOSTNAME=host.containers.internal \
          -e REDIS_PORT=''${toString config.processes.redis.ports.main.value} \
          -e IMMICH_MACHINE_LEARNING_ENABLED=false \
          -v "$DATA_DIR:/usr/src/app/upload" \
          ghcr.io/immich-app/immich-server:release
      '';

      ready.http.get = {
        host = "127.0.0.1";
        port = config.processes.immich.ports.http.value;
        path = "/api/server/ping";
        scheme = "http";
      };
      ready.initial_delay = 5;
      ready.period = 2;
      ready.timeout = 120;
    };

    # PostgreSQL database service
    services.postgres = {
      enable = true;
      listen_addresses = "*";
      package = pkgs.postgresql_16;
      extensions = ext: [ ext.pgvector ];
      initialDatabases = [
        {
          name = "immich";
          user = "immich";
          pass = "immich";
          initialSQL = ''
            ALTER USER immich WITH SUPERUSER;
            CREATE EXTENSION IF NOT EXISTS cube CASCADE;
            CREATE EXTENSION IF NOT EXISTS earthdistance CASCADE;
            CREATE EXTENSION IF NOT EXISTS vector CASCADE;
          '';
        }
      ];
      hbaConf = ''
        local all all trust
        host all all 127.0.0.1/32 trust
        host all all ::1/128 trust
        host all all all trust
      '';
    };

    # Redis service
    services.redis = {
      enable = true;
      bind = null;
    };
  };
}
