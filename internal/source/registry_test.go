package source

import (
	"reflect"
	"testing"
)

func srcs(names ...string) []*Source {
	out := make([]*Source, len(names))
	for i, n := range names {
		out[i] = &Source{Name: n}
	}
	return out
}

func TestRegistryResolve(t *testing.T) {
	one := NewRegistry(srcs("main"))
	two := NewRegistry(srcs("b", "a"))

	for _, tc := range []struct {
		name    string
		reg     *Registry
		arg     string
		want    string // source name, when no error
		wantErr string
	}{
		{"single source, empty name", one, "", "main", ""},
		{"single source, by name", one, "main", "main", ""},
		{"single source, unknown", one, "zzz", "", `unknown source "zzz"; available: main`},
		{"two sources, empty name", two, "", "", "source is required; available: a, b"},
		{"two sources, unknown", two, "zzz", "", `unknown source "zzz"; available: a, b`},
		{"two sources, by name", two, "b", "b", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.reg.Resolve(tc.arg)
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Name != tc.want {
				t.Errorf("Name = %q, want %q", got.Name, tc.want)
			}
		})
	}
}

func TestRegistryNamesSorted(t *testing.T) {
	r := NewRegistry(srcs("zeta", "alpha", "mid"))
	want := []string{"alpha", "mid", "zeta"}
	if got := r.Names(); !reflect.DeepEqual(got, want) {
		t.Errorf("Names() = %v, want %v", got, want)
	}
	// the returned slice must be a copy
	r.Names()[0] = "changed"
	if got := r.Names(); !reflect.DeepEqual(got, want) {
		t.Errorf("Names() mutated through result: %v", got)
	}
}
