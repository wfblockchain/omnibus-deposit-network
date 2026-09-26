// Package sanctions screens names against the US Treasury's OFAC Specially
// Designated Nationals list with moov-io/watchman: the Treasury's own CSV
// files (sdn.csv, add.csv, alt.csv, sdn_comments.csv) are parsed by
// watchman's OFAC reader, grouped into entities, and every screening scores
// the name against each entity with watchman's Jaro-Winkler similarity.
//
// A name-only query scores at most about 0.81 against an entity with more
// fields, because watchman discounts for fields the query does not cover,
// so the default threshold is 0.80.
package sanctions

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/go-kratos/kratos/v2/log"
	moovlog "github.com/moov-io/base/log"
	"github.com/moov-io/watchman/pkg/download"
	"github.com/moov-io/watchman/pkg/search"
	"github.com/moov-io/watchman/pkg/sources/ofac"

	"omnibus-deposit-network/services/payments-svc/internal/biz"
	"omnibus-deposit-network/services/payments-svc/internal/conf"
)

// DefaultMinMatch is the similarity at which a name is held for review.
const DefaultMinMatch = 0.80

var ofacFiles = []string{"sdn.csv", "add.csv", "alt.csv", "sdn_comments.csv"}

// Screener holds the prepared OFAC list.
type Screener struct {
	entities []search.Entity[search.Value]
	minMatch float64
	ListHash string
	LoadedAt time.Time
}

// New loads the list from c.OFACDir, downloading missing files from
// treasury.gov when c.Download is set.
func New(ctx context.Context, c *conf.Sanctions, logger log.Logger) (*Screener, error) {
	h := log.NewHelper(logger)
	start := time.Now()
	var files download.Files
	var err error
	if c.Download {
		files, err = ofac.Download(ctx, moovlog.NewNopLogger(), c.OFACDir)
	} else {
		files, err = open(c.OFACDir)
	}
	if err != nil {
		return nil, fmt.Errorf("ofac list: %w", err)
	}
	defer files.Close()
	res, err := ofac.Read(files)
	if err != nil {
		return nil, fmt.Errorf("ofac list: %w", err)
	}
	s := &Screener{minMatch: c.MinMatch, ListHash: res.ListHash, LoadedAt: time.Now()}
	if s.minMatch <= 0 {
		s.minMatch = DefaultMinMatch
	}
	for _, e := range ofac.GroupIntoEntities(res.SDNs, res.Addresses, res.SDNComments, res.AlternateIdentities) {
		s.entities = append(s.entities, e.Normalize())
	}
	h.Infof("OFAC SDN list loaded: %d entities, hash %s, in %s", len(s.entities), s.ListHash, time.Since(start).Round(time.Millisecond))
	return s, nil
}

func open(dir string) (download.Files, error) {
	out := download.Files{}
	for _, n := range ofacFiles {
		f, err := os.Open(filepath.Join(dir, n))
		if err != nil {
			out.Close()
			return nil, fmt.Errorf("%w (run `make ofac-data` or set sanctions.download)", err)
		}
		out[n] = f
	}
	return out, nil
}

func compact(s []string) []string {
	out := s[:0]
	for i, v := range s {
		if v != "" && (i == 0 || v != s[i-1]) {
			out = append(out, v)
		}
	}
	return out
}

// Entities is the number of listed entities.
func (s *Screener) Entities() int { return len(s.entities) }

// Screen scores name against every listed entity and returns the best match
// at or above the threshold.
func (s *Screener) Screen(ctx context.Context, name string) (biz.SanctionsMatch, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return biz.SanctionsMatch{}, false
	}
	q := search.Entity[search.Value]{Name: name, Type: search.EntityBusiness, Source: search.SourceAPIRequest,
		Business: &search.Business{Name: name}}.Normalize()
	best, bestScore := -1, 0.0
	for i := range s.entities {
		if ctx.Err() != nil {
			break
		}
		if sc := search.Similarity(q, s.entities[i]); sc > bestScore {
			best, bestScore = i, sc
		}
	}
	if best < 0 || bestScore < s.minMatch {
		return biz.SanctionsMatch{}, false
	}
	e := s.entities[best]
	m := biz.SanctionsMatch{List: string(e.Source), SourceID: e.SourceID, Name: e.Name, Score: bestScore}
	if sdn, ok := e.SourceData.(ofac.SDN); ok {
		m.Programs = append(m.Programs, sdn.Programs...)
	}
	if e.SanctionsInfo != nil {
		m.Programs = append(m.Programs, e.SanctionsInfo.Programs...)
	}
	sort.Strings(m.Programs)
	m.Programs = compact(m.Programs)
	return m, true
}

// ForBanks adapts the screener to the back office's receiving-bank screening,
// so a receiving bank screens inbound payees against the same list.
func (s *Screener) ForBanks() BankScreener { return BankScreener{s} }

// BankScreener implements bank.Screener.
type BankScreener struct{ s *Screener }

func (b BankScreener) Screen(name string, _ common.Address) (bool, string) {
	m, hit := b.s.Screen(context.Background(), name)
	if !hit {
		return false, ""
	}
	return true, Describe(m)
}

// Describe renders a match for an audit trail.
func Describe(m biz.SanctionsMatch) string {
	progs := ""
	if len(m.Programs) > 0 {
		progs = " (" + strings.Join(m.Programs, ", ") + ")"
	}
	return fmt.Sprintf("OFAC SDN #%s %s%s, score %.2f", m.SourceID, m.Name, progs, m.Score)
}
