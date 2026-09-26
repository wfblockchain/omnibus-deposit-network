package sanctions

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/go-kratos/kratos/v2/log"

	"omnibus-deposit-network/services/payments-svc/internal/conf"
)

var (
	once   sync.Once
	loaded *Screener
	lerr   error
)

// testList is the full OFAC SDN snapshot moov-io/watchman ships as test
// data: 16,989 real entries.
func testList(t *testing.T) *Screener {
	t.Helper()
	once.Do(func() {
		out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/moov-io/watchman").Output()
		if err != nil {
			lerr = err
			return
		}
		dir := filepath.Join(strings.TrimSpace(string(out)), "pkg", "sources", "ofac", "testdata")
		loaded, lerr = New(context.Background(), &conf.Sanctions{OFACDir: dir}, log.DefaultLogger)
	})
	if lerr != nil {
		t.Skipf("OFAC test data unavailable: %v", lerr)
	}
	return loaded
}

func TestScreensAgainstTheRealSDNList(t *testing.T) {
	s := testList(t)
	if s.Entities() < 15000 {
		t.Fatalf("only %d entities loaded", s.Entities())
	}
	for _, name := range []string{"Aerocaribbean Airlines", "BANCO NACIONAL DE CUBA"} {
		m, hit := s.Screen(context.Background(), name)
		if !hit {
			t.Errorf("%s is on the SDN list and was not caught", name)
			continue
		}
		t.Logf("%s → %s", name, Describe(m))
	}
	m, _ := s.Screen(context.Background(), "Aerocaribbean Airlines")
	if m.SourceID != "36" || !strings.Contains(strings.Join(m.Programs, ","), "CUBA") {
		t.Fatalf("wrong entry: %+v", m)
	}
	for _, name := range []string{"Northwind Corp", "Contoso Ltd", "Fabrikam Inc", "Dave Ltd", "Tailspin Toys"} {
		if m, hit := s.Screen(context.Background(), name); hit {
			t.Errorf("false positive: %s → %s", name, Describe(m))
		}
	}
	if hit, why := s.ForBanks().Screen("Aerocaribbean Airlines", [20]byte{}); !hit || !strings.Contains(why, "SDN #36") {
		t.Fatalf("receiving-bank adapter: %v %q", hit, why)
	}
}
