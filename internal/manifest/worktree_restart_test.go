package manifest

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestWorktreeRestartPoliciesPreserveSuspensionAndResources(t *testing.T) {
	root := project(t, `x-site-deploy:
  domain: worktree-restart.localhost
  service: web
  port: 8080
services:
  web:
    image: nginx:alpine
    restart: always
    cpus: 0.5
    mem_limit: 128m
    deploy:
      resources:
        limits:
          cpus: "0.5"
          memory: 128m
        reservations:
          memory: 32m
  database:
    image: postgres:17-alpine
    restart: always
    cpus: 0.25
    mem_limit: 256m
    deploy:
      resources:
        limits:
          cpus: "0.25"
          memory: 256m
        reservations:
          memory: 64m
`)
	_, filename, err := Prepare(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	services := generated(t, filename)["services"].(map[string]any)
	for _, want := range []struct {
		name, cpuLimit, memoryLimit, memoryReservation string
		cpus                                           float64
	}{
		{"web", "0.5", "128m", "32m", 0.5},
		{"database", "0.25", "256m", "64m", 0.25},
	} {
		service := services[want.name].(map[string]any)
		if service["restart"] != "unless-stopped" {
			t.Errorf("%s restart = %v; idle suspension must survive a Docker restart", want.name, service["restart"])
		}
		if service["cpus"] != want.cpus || service["mem_limit"] != want.memoryLimit {
			t.Errorf("%s service resource limits changed: %v", want.name, service)
		}
		resources := service["deploy"].(map[string]any)["resources"]
		expected := map[string]any{
			"limits":       map[string]any{"cpus": want.cpuLimit, "memory": want.memoryLimit},
			"reservations": map[string]any{"memory": want.memoryReservation},
		}
		if !reflect.DeepEqual(resources, expected) {
			t.Errorf("%s deployment resources changed: got %v, want %v", want.name, resources, expected)
		}
	}
}

func TestWorktreeRestartSupportedPoliciesRemainExplicit(t *testing.T) {
	for _, policy := range []string{"", "no", "on-failure", "on-failure:0", "on-failure:3", "unless-stopped"} {
		name := policy
		if name == "" {
			name = "absent"
		}
		t.Run(name, func(t *testing.T) {
			compose := basic
			if policy != "" {
				compose += "    restart: " + strconv.Quote(policy) + "\n"
			}
			_, filename, err := Prepare(project(t, compose), nil)
			if err != nil {
				t.Fatal(err)
			}
			web := generated(t, filename)["services"].(map[string]any)["web"].(map[string]any)
			got, exists := web["restart"]
			if policy == "" {
				if exists {
					t.Fatalf("absent restart policy became %v", got)
				}
			} else if !exists || got != policy {
				t.Fatalf("restart policy = %v, want %q", got, policy)
			}
		})
	}
}

func TestWorktreeRestartRejectsInvalidPoliciesBeforeDocker(t *testing.T) {
	for _, value := range []string{
		`""`, `null`, `false`, `1`, `[]`, `{condition: any}`,
		`"${RESTART_POLICY:-always}"`, `"$RESTART_POLICY"`, `"$$RESTART_POLICY"`,
		`"on-failure:"`, `"on-failure:-1"`, `"on-failure:+1"`, `"on-failure:1.5"`,
		`"on-failure:1:2"`, `"on-failure:999999999999999999999999999999"`,
		`"on-failure: 3"`, `"always "`, `"ALWAYS"`, `"never"`,
	} {
		t.Run(value, func(t *testing.T) {
			_, filename, err := Prepare(project(t, basic+"    restart: "+value+"\n"), nil)
			if err == nil || !strings.Contains(err.Error(), "services.web.restart") {
				t.Fatalf("invalid policy %s: filename=%q, err=%v", value, filename, err)
			}
			if filename != "" {
				t.Fatalf("invalid policy produced a Docker manifest: %q", filename)
			}
		})
	}
}

func TestWorktreeRestartRejectsDeployOverrideOnEveryService(t *testing.T) {
	for _, service := range []string{"web", "database"} {
		for _, policy := range []string{"{condition: any}", "{condition: none}", "{condition: on-failure}", "{}", "null"} {
			t.Run(service+"/"+policy, func(t *testing.T) {
				compose := basic
				if service == "database" {
					compose += "  database:\n    image: postgres:17-alpine\n"
				}
				compose += "    restart: unless-stopped\n    deploy:\n      restart_policy: " + policy + "\n"
				_, filename, err := Prepare(project(t, compose), nil)
				location := fmt.Sprintf("services.%s.deploy.restart_policy", service)
				if err == nil || !strings.Contains(err.Error(), location) {
					t.Fatalf("deploy restart override %s: filename=%q, err=%v", policy, filename, err)
				}
				if filename != "" {
					t.Fatalf("restart override produced a Docker manifest: %q", filename)
				}
			})
		}
	}
}
