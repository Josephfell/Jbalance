package admin

import (
	"reflect"
	"testing"

	"github.com/Josephfell/Jbalance/internal/controlplane"
)

func TestParseMatch(t *testing.T) {
	tests := []struct {
		name        string
		in          string
		wantHeaders []controlplane.HeaderMatch
		wantQueries []controlplane.QueryMatch
	}{
		{
			name: "empty",
			in:   "",
		},
		{
			name:        "header equals",
			in:          "header:X-Api-Version: 2",
			wantHeaders: []controlplane.HeaderMatch{{Name: "X-Api-Version", Value: "2"}},
		},
		{
			name:        "header presence only",
			in:          "header:X-Debug",
			wantHeaders: []controlplane.HeaderMatch{{Name: "X-Debug"}},
		},
		{
			name:        "bare line treated as header",
			in:          "X-From: edge",
			wantHeaders: []controlplane.HeaderMatch{{Name: "X-From", Value: "edge"}},
		},
		{
			name:        "query equals",
			in:          "query:canary=true",
			wantQueries: []controlplane.QueryMatch{{Name: "canary", Value: "true"}},
		},
		{
			name:        "query presence only",
			in:          "query:debug",
			wantQueries: []controlplane.QueryMatch{{Name: "debug"}},
		},
		{
			name:        "mixed multiline",
			in:          "header:X-Api-Version: 2\nquery:canary=true\n\nheader:X-Debug",
			wantHeaders: []controlplane.HeaderMatch{{Name: "X-Api-Version", Value: "2"}, {Name: "X-Debug"}},
			wantQueries: []controlplane.QueryMatch{{Name: "canary", Value: "true"}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotH, gotQ := parseMatch(tc.in)
			if !reflect.DeepEqual(gotH, tc.wantHeaders) {
				t.Errorf("headers: got %+v, want %+v", gotH, tc.wantHeaders)
			}
			if !reflect.DeepEqual(gotQ, tc.wantQueries) {
				t.Errorf("queries: got %+v, want %+v", gotQ, tc.wantQueries)
			}
		})
	}
}

func TestFormatMatch_RoundTrips(t *testing.T) {
	headers := []controlplane.HeaderMatch{
		{Name: "X-Api-Version", Value: "2"},
		{Name: "X-Debug"},
	}
	queries := []controlplane.QueryMatch{
		{Name: "canary", Value: "true"},
		{Name: "debug"},
	}

	formatted := formatMatch(headers, queries)
	gotH, gotQ := parseMatch(formatted)

	if !reflect.DeepEqual(gotH, headers) {
		t.Errorf("headers did not round-trip: got %+v, want %+v (formatted=%q)", gotH, headers, formatted)
	}
	if !reflect.DeepEqual(gotQ, queries) {
		t.Errorf("queries did not round-trip: got %+v, want %+v (formatted=%q)", gotQ, queries, formatted)
	}
}

func TestFormatMatch_Empty(t *testing.T) {
	if got := formatMatch(nil, nil); got != "" {
		t.Errorf("expected empty string for no conditions, got %q", got)
	}
}
