package manifest

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestPrepareHostPathsNeedApproval(t *testing.T) {
	compose := basic + `    volumes:
      - /srv/media//films:/films:ro
      - type: bind
        source: /srv/uploads
        target: /uploads
        bind:
          create_host_path: true
      - ./public:/public:ro
`
	root := project(t, compose)
	if err := os.Mkdir(root+"/public", 0755); err != nil {
		t.Fatal(err)
	}
	type call struct {
		source   string
		readOnly bool
	}
	var calls []call
	approve := func(source string, readOnly bool) (string, error) {
		calls = append(calls, call{source, readOnly})
		return strings.ReplaceAll(source, "//", "/"), nil
	}
	_, filename, err := Prepare(root, approve)
	if err != nil {
		t.Fatal(err)
	}
	if want := []call{{"/srv/media//films", true}, {"/srv/uploads", false}}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("checks = %+v; want %+v", calls, want)
	}
	volumes := generated(t, filename)["services"].(map[string]any)["web"].(map[string]any)["volumes"].([]any)
	films, uploads := volumes[0].(map[string]any), volumes[1].(map[string]any)
	if films["source"] != "/srv/media/films" || films["read_only"] != true ||
		uploads["source"] != "/srv/uploads" || uploads["bind"].(map[string]any)["create_host_path"] != false {
		t.Fatalf("generated volumes: %+v", volumes)
	}
	data, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	mounts, err := HostMounts(data)
	want := []HostMount{{"web", "/srv/media/films", true}, {"web", "/srv/uploads", false}}
	if err != nil || !reflect.DeepEqual(mounts, want) {
		t.Fatalf("HostMounts = %+v, %v; want %+v", mounts, err, want)
	}

	// Without approval, or when it's refused, the deploy fails and names the mount.
	if _, _, err := Prepare(project(t, basic+"    volumes: [\"/srv/media:/media:ro\"]\n"), nil); err == nil ||
		!strings.Contains(err.Error(), "services.web.volumes[0].source: host path /srv/media is not allowed") {
		t.Errorf("nil check: %v", err)
	}
	refuse := func(string, bool) (string, error) {
		return "", errors.New("host path /srv/media is not allowed for app")
	}
	if _, _, err := Prepare(project(t, basic+"    volumes: [\"/srv/media:/media:ro\"]\n"), refuse); err == nil ||
		!strings.Contains(err.Error(), "volumes[0].source: host path /srv/media is not allowed for app") {
		t.Errorf("refused: %v", err)
	}
	// Home-relative paths are neither bundle files nor host paths.
	if _, _, err := Prepare(project(t, basic+"    volumes: [\"~/media:/media:ro\"]\n"), approve); err == nil {
		t.Error("accepted a ~ path")
	}
}

func TestHostMountsReadsShortSyntaxAndSkipsOthers(t *testing.T) {
	data := []byte(`{"services":{"b":{"volumes":["/srv/b:/b:ro","data:/data"]},"a":{"volumes":[
		{"type":"bind","source":"/srv/a","target":"/a"},
		{"type":"bind","source":"./public","target":"/public"},
		{"type":"volume","source":"data","target":"/data"},
		{"type":"tmpfs","target":"/tmp"}]}}}`)
	mounts, err := HostMounts(data)
	want := []HostMount{{"a", "/srv/a", false}, {"b", "/srv/b", true}}
	if err != nil || !reflect.DeepEqual(mounts, want) {
		t.Fatalf("HostMounts = %+v, %v; want %+v", mounts, err, want)
	}
	if _, err := HostMounts([]byte(`{"services":`)); err == nil {
		t.Error("accepted invalid JSON")
	}
}
