package manifest

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

const volumesBundle = `x-site-deploy:
  domain: example.localhost
  service: web
  port: 8080
  volumes:
    corpus:
      key: sdv_corpus-key
      target: /corpus/
      read_only: true
      snapshot: true
    cache:
      key: sdv_cache-key
      target: /cache
      service: worker
services:
  web:
    image: nginx:alpine
    volumes:
      - ./public:/usr/share/nginx/html:ro
  worker:
    image: busybox:1.37
`

type volumeCall struct {
	name, key string
	snapshot  bool
}

func TestPrepareMountsVolumesThatTheirCheckApproves(t *testing.T) {
	root := project(t, volumesBundle)
	if err := os.Mkdir(root+"/public", 0755); err != nil {
		t.Fatal(err)
	}
	var calls []volumeCall
	check := func(name, key string, snapshot bool) (string, error) {
		calls = append(calls, volumeCall{name, key, snapshot})
		return "/srv/volumes/" + name + "/data", nil
	}
	_, filename, err := PrepareWith(root, Checks{Volume: check})
	if err != nil {
		t.Fatal(err)
	}
	if want := []volumeCall{{"cache", "sdv_cache-key", false}, {"corpus", "sdv_corpus-key", true}}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("checks %+v; want %+v", calls, want)
	}
	doc := generated(t, filename)
	if _, exists := doc["x-site-deploy"]; exists {
		t.Fatal("the generated file keeps x-site-deploy, with its keys")
	}
	services := doc["services"].(map[string]any)
	web := services["web"].(map[string]any)["volumes"].([]any)
	worker := services["worker"].(map[string]any)["volumes"].([]any)
	wantWeb := map[string]any{"type": "bind", "source": "/srv/volumes/corpus/data", "target": "/corpus", "read_only": true, "bind": map[string]any{"create_host_path": false}}
	wantWorker := map[string]any{"type": "bind", "source": "/srv/volumes/cache/data", "target": "/cache", "read_only": false, "bind": map[string]any{"create_host_path": false}}
	if len(web) != 2 || !reflect.DeepEqual(web[1], wantWeb) || len(worker) != 1 || !reflect.DeepEqual(worker[0], wantWorker) {
		t.Fatalf("web volumes %+v\nworker volumes %+v", web, worker)
	}
	data, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	mounts, err := HostMounts(data)
	want := []HostMount{{"web", "/srv/volumes/corpus/data", true}, {"worker", "/srv/volumes/cache/data", false}}
	if err != nil || !reflect.DeepEqual(mounts, want) {
		t.Fatalf("HostMounts = %+v %v", mounts, err)
	}
}

func TestPrepareRejectsInvalidVolumesBeforeChecking(t *testing.T) {
	approve := func(name, key string, snapshot bool) (string, error) {
		t.Errorf("checked %s for an invalid bundle", name)
		return "/srv/volumes/" + name + "/data", nil
	}
	for _, test := range []struct{ volumes, services, want string }{
		{"    corpus: {target: /corpus}\n", "", "x-site-deploy.volumes.corpus.key must be the volume's key"},
		{"    corpus: {key: 1234, target: /corpus}\n", "", "x-site-deploy.volumes.corpus.key must be the volume's key"},
		{"    corpus: {key: k, target: corpus}\n", "", "x-site-deploy.volumes.corpus.target must be an absolute container path"},
		{"    corpus: {key: k}\n", "", "x-site-deploy.volumes.corpus.target must be a nonempty string"},
		{"    corpus: {key: k, target: /c, read_only: yes please}\n", "", "x-site-deploy.volumes.corpus.read_only must be a boolean"},
		{"    corpus: {key: k, target: /c, snapshot: 1}\n", "", "x-site-deploy.volumes.corpus.snapshot must be a boolean"},
		{"    corpus: {key: k, target: /c, subpath: x}\n", "", "x-site-deploy.volumes.corpus.subpath is not supported"},
		{"    corpus: {key: k, target: /c, service: db}\n", "", `x-site-deploy.volumes.corpus.service "db" does not exist`},
		{"    Corpus: {key: k, target: /c}\n", "", "x-site-deploy.volumes.Corpus: volume names start with a lowercase letter"},
		{"    corpus: {key: k, target: /data}\n", "    volumes: [\"data:/data\"]\n", "x-site-deploy.volumes.corpus.target /data is already mounted in service web"},
		{"    corpus: {key: k, target: /c}\n    other: {key: k, target: /c/}\n", "", "x-site-deploy.volumes.other.target /c is already mounted in service web"},
		{"    corpus: {key: k, target: /c}\n", "    privileged: true\n", "privileged containers are not supported"},
		{"    {}\n", "", "x-site-deploy.volumes must map volume names to mounts"},
	} {
		compose := strings.Replace(basic, "  port: 8080\n", "  port: 8080\n  volumes:\n"+test.volumes, 1) + test.services
		if strings.Contains(test.services, "data:/data") {
			compose += "volumes:\n  data: {}\n"
		}
		if test.volumes == "    {}\n" {
			compose = strings.Replace(basic, "  port: 8080\n", "  port: 8080\n  volumes: {}\n", 1)
		}
		_, _, err := PrepareWith(project(t, compose), Checks{Volume: approve})
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("volumes:\n%s=> %v; want %q", test.volumes, err, test.want)
		}
	}
}

func TestPrepareVolumesNeedAServerThatHasThem(t *testing.T) {
	compose := strings.Replace(basic, "  port: 8080\n", "  port: 8080\n  volumes:\n    corpus: {key: k, target: /corpus}\n", 1)
	if _, _, err := Prepare(project(t, compose), nil); err == nil || !strings.Contains(err.Error(), "serve --volumes-dir") {
		t.Fatalf("without volumes: %v", err)
	}
	refuse := func(string, string, bool) (string, error) { return "", errors.New("wrong volume key for corpus") }
	if _, _, err := PrepareWith(project(t, compose), Checks{Volume: refuse}); err == nil ||
		!strings.Contains(err.Error(), "x-site-deploy.volumes.corpus: wrong volume key for corpus") || strings.Contains(err.Error(), "key: k") {
		t.Fatalf("refused: %v", err)
	}
}
