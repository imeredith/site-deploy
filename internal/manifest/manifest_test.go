package manifest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const basic = `x-site-deploy:
  domain: example.localhost
  service: web
  port: 8080
services:
  web:
    image: nginx:alpine
`

func project(t *testing.T, compose string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "compose.yaml"), []byte(compose), 0600); err != nil {
		t.Fatal(err)
	}
	return root
}

func generated(t *testing.T, filename string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestPrepareNormalProject(t *testing.T) {
	root := project(t, `x-site-deploy:
  domain: EXAMPLE.localhost.
  service: web
  port: 8080
services:
  web:
    image: nginx:alpine
    ports: ["0.0.0.0:8080:8080", "8443:443"]
    command: ["sh", "-c", "echo $$HOSTNAME"]
    volumes:
      - ./public:/usr/share/nginx/html:ro
      - type: bind
        source: app.conf
        target: /etc/app.conf
        bind:
          create_host_path: true
    env_file:
      - path: ./app.env
        format: raw
    configs: [app-config]
    secrets:
      - source: password
        target: password
    networks: [private]
    depends_on:
      db:
        condition: service_healthy
    mem_limit: 128m
    cpus: 0.5
    deploy:
      resources:
        limits:
          memory: 128m
          cpus: "0.5"
        reservations:
          memory: 32m
  db:
    image: postgres:17-alpine
    ports:
      - target: 5432
        published: "5432"
        host_ip: 0.0.0.0
    volumes: ["data:/var/lib/postgresql/data"]
    networks: [private]
    environment:
      POSTGRES_PASSWORD: password
    healthcheck:
      test: [CMD-SHELL, "pg_isready -U postgres"]
      interval: 5s
volumes:
  data:
    driver: local
networks:
  private:
    driver: bridge
configs:
  app-config:
    file: ./app.conf
secrets:
  password:
    file: ./password.txt
`)
	if err := os.Mkdir(filepath.Join(root, "public"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"app.conf", "app.env", "password.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("value=hello"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	spec, output, err := Prepare(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Domain != "example.localhost" || spec.IdleTimeout != "5m" || spec.StartTimeout != "60s" || spec.SuspendMode != "stop" {
		t.Fatalf("wrong normalized defaults: %+v", spec)
	}
	if !filepath.IsAbs(output) || filepath.Base(output) != generatedName {
		t.Fatalf("unexpected generated file: %s", output)
	}
	doc := generated(t, output)
	if _, exists := doc["x-site-deploy"]; exists {
		t.Fatal("deployment metadata must not enter the runtime file")
	}
	services := doc["services"].(map[string]any)
	web := services["web"].(map[string]any)
	ports := web["ports"].([]any)
	if len(ports) != 1 {
		t.Fatalf("expected exactly one routed port, got %v", ports)
	}
	port := ports[0].(map[string]any)
	if port["target"] != float64(8080) || port["host_ip"] != "127.0.0.1" || port["protocol"] != "tcp" {
		t.Fatalf("unsafe or incorrect generated port: %v", port)
	}
	if _, exists := port["published"]; exists {
		t.Fatal("generated port must be allocated ephemerally")
	}
	if _, exists := services["db"].(map[string]any)["ports"]; exists {
		t.Fatal("secondary service retained host publication")
	}
	if web["mem_limit"] != "128m" || web["cpus"] != 0.5 {
		t.Fatal("resource limits were not preserved")
	}
	if web["command"].([]any)[2] != "echo $$HOSTNAME" {
		t.Fatal("command dollar escaping was changed")
	}
	for _, entry := range web["volumes"].([]any) {
		mount := entry.(map[string]any)
		if mount["type"] != "bind" || mount["bind"].(map[string]any)["create_host_path"] != false {
			t.Fatalf("bind must disable automatic host path creation: %v", mount)
		}
		if filepath.IsAbs(mount["source"].(string)) {
			t.Fatal("prepared bind paths must survive renaming a staging directory")
		}
	}
	mode, err := os.Stat(output)
	if err != nil || mode.Mode().Perm() != 0600 {
		t.Fatalf("generated manifest must be private: %v, %v", mode, err)
	}
}

func TestPrepareIdleDisabledAndPause(t *testing.T) {
	root := project(t, strings.Replace(basic, "  port: 8080", "  port: 8080\n  idle_timeout: 0\n  start_timeout: 500ms\n  suspend_mode: pause", 1))
	spec, _, err := Prepare(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if spec.IdleDuration() != 0 || spec.SuspendMode != "pause" {
		t.Fatalf("unexpected spec: %+v", spec)
	}
}

func TestPrepareInvalidExtension(t *testing.T) {
	cases := []struct {
		name, from, to, contains string
	}{
		{"domain scheme", "example.localhost", "https://example.com", "domain"},
		{"domain port", "example.localhost", "example.com:443", "domain"},
		{"domain wildcard", "example.localhost", "'*.example.com'", "domain"},
		{"domain IP", "example.localhost", "127.0.0.1", "domain"},
		{"domain empty label", "example.localhost", "example..localhost", "domain"},
		{"domain edge hyphen", "example.localhost", "-example.localhost", "domain"},
		{"unknown service", "service: web", "service: missing", "does not exist"},
		{"invalid service", "service: web", "service: ${SERVICE}", "service"},
		{"low port", "port: 8080", "port: 0", "port"},
		{"high port", "port: 8080", "port: 65536", "port"},
		{"string port", "port: 8080", "port: '8080'", "port"},
		{"negative idle", "port: 8080", "port: 8080\n  idle_timeout: -1s", "idle_timeout"},
		{"invalid idle", "port: 8080", "port: 8080\n  idle_timeout: soon", "idle_timeout"},
		{"zero start", "port: 8080", "port: 8080\n  start_timeout: 0", "start_timeout"},
		{"unknown mode", "port: 8080", "port: 8080\n  suspend_mode: freeze", "suspend_mode"},
		{"unknown field", "port: 8080", "port: 8080\n  idle_timout: 5s", "idle_timout"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := project(t, strings.Replace(basic, tc.from, tc.to, 1))
			_, output, err := Prepare(root, nil)
			if err == nil || !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("expected error containing %q, got %v", tc.contains, err)
			}
			if output != "" {
				t.Fatal("invalid manifests must not return a runtime file")
			}
			if _, err := os.Stat(filepath.Join(root, generatedName)); !os.IsNotExist(err) {
				t.Fatalf("invalid manifests must not be written: %v", err)
			}
		})
	}
}

func TestPrepareRejectsHostCapabilities(t *testing.T) {
	cases := []struct{ name, field string }{
		{"build", "build: ."},
		{"extends", "extends: {file: ../compose.yaml, service: web}"},
		{"profiles", "profiles: [debug]"},
		{"container name", "container_name: shared"},
		{"host networking", "network_mode: host"},
		{"container namespace", "network_mode: container:existing"},
		{"pid", "pid: host"},
		{"ipc", "ipc: host"},
		{"privileged", "privileged: true"},
		{"device", "devices: [/dev/sda]"},
		{"capability", "cap_add: [SYS_ADMIN]"},
		{"docker socket", "use_api_socket: true"},
		{"lifecycle hook", "post_start: [{command: echo hello, privileged: true}]"},
		{"external provider", "provider: {type: evil}"},
		{"volume inheritance", "volumes_from: [existing]"},
		{"external link", "external_links: [existing]"},
		{"unconfined", "security_opt: [seccomp:unconfined]"},
		{"privileged runtime", "runtime: custom"},
		{"network driver options", "networks: {default: {driver_opts: {parent: eth0}}}"},
		{"GPU reservations", "deploy: {resources: {reservations: {devices: [{capabilities: [gpu]}]}}}"},
		{"routed replicas", "deploy: {replicas: 2}"},
		{"routed scale", "scale: 2"},
		{"build pull policy", "pull_policy: build"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := Prepare(project(t, basic+"    "+tc.field+"\n"), nil)
			if err == nil {
				t.Fatalf("accepted unsafe field: %s", tc.field)
			}
		})
	}
}

func TestPrepareRejectsMountEscapes(t *testing.T) {
	cases := []string{
		"'/etc:/data:ro'",
		"'../outside:/data'",
		"'./public/../../outside:/data'",
		"'~/secret:/data'",
		"'${HOME}:/data'",
		"'./missing:/data'",
		"{type: bind, source: /var/run/docker.sock, target: /socket}",
		"{type: bind, source: '../outside', target: /data}",
		"{type: bind, source: './public', target: /data, bind: {propagation: rshared}}",
		"{type: volume, source: unknown, target: /data}",
		"{type: image, source: alpine, target: /data}",
	}
	for _, mount := range cases {
		t.Run(mount, func(t *testing.T) {
			root := project(t, basic+"    volumes: ["+mount+"]\n")
			if err := os.Mkdir(filepath.Join(root, "public"), 0755); err != nil {
				t.Fatal(err)
			}
			if _, _, err := Prepare(root, nil); err == nil {
				t.Fatalf("accepted unsafe mount: %s", mount)
			}
		})
	}
}

func TestPrepareRejectsSymlinkSources(t *testing.T) {
	root := project(t, basic+"    volumes: ['./linked:/data']\n")
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Prepare(root, nil); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected symlink rejection, got %v", err)
	}
	root = project(t, basic+"    env_file: ./linked/value.env\n")
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Prepare(root, nil); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected parent symlink rejection, got %v", err)
	}
}

func TestPrepareRejectsResourceEscapes(t *testing.T) {
	cases := []string{
		"volumes: {data: {external: true}}",
		"volumes: {data: {name: shared-data}}",
		"volumes: {data: {driver: local, driver_opts: {type: none, o: bind, device: /etc}}}",
		"volumes: {data: {driver: third-party}}",
		"networks: {default: {external: true}}",
		"networks: {default: {driver: host}}",
		"networks: {default: {name: host-network}}",
		"networks: {default: {driver_opts: {parent: eth0}}}",
		"networks: {default: {ipam: {driver: third-party}}}",
		"configs: {app: {file: /etc/passwd}}",
		"configs: {app: {file: '${HOME}/password'}}",
		"secrets: {password: {file: ../secret}}",
		"secrets: {password: {external: true}}",
		"secrets: {password: {environment: HOST_SECRET}}",
		"include: [/etc/compose.yaml]",
		"name: shared-project",
	}
	for _, extra := range cases {
		t.Run(extra, func(t *testing.T) {
			if _, _, err := Prepare(project(t, basic+extra+"\n"), nil); err == nil {
				t.Fatalf("accepted unsafe resource: %s", extra)
			}
		})
	}
}

func TestPrepareRejectsFileEscapesAndEnvironmentInheritance(t *testing.T) {
	for _, field := range []string{
		"env_file: /etc/environment",
		"env_file: ../outside.env",
		"env_file: ${ENV_FILE}",
		"env_file: {path: ./missing.env}",
		"env_file: [{path: ./missing.env, required: false}]",
		"label_file: /etc/passwd",
		"environment: [HOST_SECRET]",
		"environment: {HOST_SECRET: null}",
	} {
		t.Run(field, func(t *testing.T) {
			if _, _, err := Prepare(project(t, basic+"    "+field+"\n"), nil); err == nil {
				t.Fatalf("accepted unsafe field: %s", field)
			}
		})
	}
}

func TestPrepareComposeDocumentValidation(t *testing.T) {
	for _, text := range []string{
		basic + "---\nservices: {}\n",
		basic + "services: {}\n",
		strings.Replace(basic, "nginx:alpine", "${APP_IMAGE}", 1),
		strings.Replace(basic, "nginx:alpine", "!reset null", 1),
		"[]\n",
		"null\n",
	} {
		t.Run(text, func(t *testing.T) {
			if _, _, err := Prepare(project(t, text), nil); err == nil {
				t.Fatal("invalid YAML accepted")
			}
		})
	}
	t.Run("multiple root files", func(t *testing.T) {
		root := project(t, basic)
		if err := os.WriteFile(filepath.Join(root, "docker-compose.yml"), []byte(basic), 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := Prepare(root, nil); err == nil {
			t.Fatal("ambiguous root Compose files accepted")
		}
	})
	t.Run("missing root file", func(t *testing.T) {
		if _, _, err := Prepare(t.TempDir(), nil); err == nil {
			t.Fatal("missing root Compose file accepted")
		}
	})
	t.Run("compose symlink", func(t *testing.T) {
		root := t.TempDir()
		original := project(t, basic)
		if err := os.Symlink(filepath.Join(original, "compose.yaml"), filepath.Join(root, "compose.yaml")); err != nil {
			t.Fatal(err)
		}
		if _, _, err := Prepare(root, nil); err == nil {
			t.Fatal("symlink Compose file accepted")
		}
	})
}

func TestPrepareAnchorsRemainValidated(t *testing.T) {
	root := project(t, `x-site-deploy: {domain: example.localhost, service: web, port: 80}
x-defaults: &defaults
  image: nginx:alpine
  ports: ["80:80"]
services:
  web:
    <<: *defaults
`)
	_, output, err := Prepare(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	ports := generated(t, output)["services"].(map[string]any)["web"].(map[string]any)["ports"].([]any)
	if len(ports) != 1 || ports[0].(map[string]any)["host_ip"] != "127.0.0.1" {
		t.Fatalf("ports inherited through anchors escaped validation: %v", ports)
	}
	unsafe := `x-site-deploy: {domain: example.localhost, service: web, port: 80}
x-defaults: &defaults
  image: nginx:alpine
  privileged: true
services:
  web:
    <<: *defaults
`
	if _, _, err := Prepare(project(t, unsafe), nil); err == nil {
		t.Fatal("privileged option inherited from anchor accepted")
	}
}

func TestPrepareDoesNotFollowGeneratedFileSymlink(t *testing.T) {
	root := project(t, basic)
	out := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(out, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(out, filepath.Join(root, generatedName)); err != nil {
		t.Fatal(err)
	}
	_, file, err := Prepare(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(out)
	if err != nil || string(contents) != "unchanged" {
		t.Fatalf("followed generated-file symlink: %q, %v", contents, err)
	}
	if info, err := os.Lstat(file); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("generated manifest is not a regular file: %v, %v", info, err)
	}
}

const testHash = "$2a$14$O4UeNfvX7xUhP9mj6tjWIOkvvB/sTqKipfaXtgWqmJ7MZjMg/l9me"

func TestPrepareBasicAuth(t *testing.T) {
	root := project(t, `x-site-deploy:
  domain: private.localhost
  service: web
  port: 8080
  basic_auth:
    - user: preview
      hash: `+testHash+`
    - user: alice@example.com
      hash: "`+testHash+`"
services:
  web:
    image: nginx:alpine
`)
	spec, filename, err := Prepare(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.BasicAuth) != 2 || spec.BasicAuth[0].User != "preview" || spec.BasicAuth[0].Hash != testHash || spec.BasicAuth[1].User != "alice@example.com" {
		t.Fatalf("basic auth not parsed literally: %+v", spec.BasicAuth)
	}
	// Compose never sees the accounts, so the $ in hashes is never interpolated.
	data, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "basic_auth") || strings.Contains(string(data), "$2a$") {
		t.Fatalf("generated Compose contains credentials: %s", data)
	}
	if spec, _, err := Prepare(project(t, basic), nil); err != nil || spec.BasicAuth != nil {
		t.Fatalf("public app gained basic auth: %+v %v", spec.BasicAuth, err)
	}
}

func TestPrepareRejectsInvalidBasicAuth(t *testing.T) {
	header := "x-site-deploy:\n  domain: private.localhost\n  service: web\n  port: 8080\n  basic_auth:"
	services := "\nservices:\n  web:\n    image: nginx:alpine\n"
	many := ""
	for i := 0; i < 65; i++ {
		many += fmt.Sprintf("\n    - {user: u%d, hash: %q}", i, testHash)
	}
	for name, test := range map[string]struct{ auth, problem string }{
		"empty":       {" []", "nonempty list"},
		"mapping":     {" {user: preview}", "nonempty list"},
		"plaintext":   {"\n    - {user: preview, password: secret}", "password is not supported"},
		"no hash":     {"\n    - {user: preview}", "string user and hash"},
		"not bcrypt":  {"\n    - {user: preview, hash: secret}", "bcrypt hash"},
		"escaped":     {"\n    - {user: preview, hash: \"$$2a$$14$$O4UeNfvX7xUhP9mj6tjWIOkvvB/sTqKipfaXtgWqmJ7MZjMg/l9me\"}", "without Compose $$ escaping"},
		"colon user":  {"\n    - {user: \"a:b\", hash: \"" + testHash + "\"}", "basic auth user"},
		"duplicate":   {"\n    - {user: preview, hash: \"" + testHash + "\"}\n    - {user: preview, hash: \"" + testHash + "\"}", "duplicate user"},
		"numeric":     {"\n    - {user: 123, hash: \"" + testHash + "\"}", "string user and hash"},
		"too many":    {many, "at most 64"},
		"extra field": {"\n    - {user: preview, hash: \"" + testHash + "\", realm: x}", "realm is not supported"},
	} {
		_, _, err := Prepare(project(t, header+test.auth+services), nil)
		if err == nil || !strings.Contains(err.Error(), test.problem) {
			t.Errorf("%s: got %v, want error containing %q", name, err, test.problem)
		}
	}
}
