package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTokenGeneratedOnceAndPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	first, created, err := loadToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if !created || len(first) != 64 {
		t.Fatal("expected new random 256-bit token")
	}
	second, created, err := loadToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if created || first != second {
		t.Fatal("token changed across daemon restart")
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadToken(path); err == nil {
		t.Fatal("accepted token readable by other users")
	}
}

func TestControlAndGatewayAddressesMustBeLoopback(t *testing.T) {
	for _, address := range []string{"127.0.0.1:9081", "[::1]:9081"} {
		if err := loopbackAddress(address); err != nil {
			t.Errorf("%s: %v", address, err)
		}
	}
	for _, address := range []string{":9081", "0.0.0.0:9081", "192.0.2.1:9081", "example.com:9081", "localhost:9081"} {
		if err := loopbackAddress(address); err == nil {
			t.Errorf("accepted %s", address)
		}
	}
}
