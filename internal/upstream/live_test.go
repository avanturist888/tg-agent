//go:build live

package upstream

import (
	"context"
	"testing"
)

// go test -tags live -run Live ./internal/upstream -v — настоящие источники.
func TestLiveSources(t *testing.T) {
	st := &State{Sources: map[string]*SourceState{}}
	for _, r := range Check(context.Background(), NewClient(), st, Sources()) {
		if r.Err != nil {
			t.Errorf("%s: %v", r.Source.ID, r.Err)
			continue
		}
		for _, l := range r.Source.Labels {
			t.Logf("%s · %s: %s", r.Source.Title, l.Text, r.Cur[l.Key])
		}
	}
}
