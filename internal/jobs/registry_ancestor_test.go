package jobs

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestRegistrySynchronizesExistingAndNewAncestors(t *testing.T) {
	root := filepath.Join(t.TempDir(), "new", "nested", "jobs")
	for attempt := range 2 {
		var observed []string
		registry, err := openRegistry(t.Context(), root, func(parent string) error {
			observed = append(observed, parent)
			return syncDirectory(parent)
		})
		if err != nil {
			t.Fatal(err)
		}
		canonical := registry.root
		if err := registry.Close(); err != nil {
			t.Fatal(err)
		}
		var expected []string
		for parent := filepath.Dir(canonical); ; parent = filepath.Dir(parent) {
			expected = append(expected, parent)
			if filepath.Dir(parent) == parent {
				break
			}
		}
		if !reflect.DeepEqual(observed, expected) {
			t.Fatalf("attempt %d ancestors=%v want %v", attempt, observed, expected)
		}
	}
}

func TestRegistryAncestorFailurePreventsAdmissionDatabase(t *testing.T) {
	for _, depth := range []int{0, 1, 2} {
		t.Run(string(rune('0'+depth)), func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "new", "nested", "jobs")
			failure := errors.New("ancestor fsync failed")
			calls := 0
			registry, err := openRegistry(t.Context(), root, func(parent string) error {
				current := calls
				calls++
				if current == depth {
					return failure
				}
				return syncDirectory(parent)
			})
			if registry != nil || !errors.Is(err, failure) {
				t.Fatalf("registry=%v err=%v", registry, err)
			}
			if _, statErr := os.Stat(filepath.Join(root, "admissions.sqlite")); !os.IsNotExist(statErr) {
				t.Fatalf("database existed before ancestor barrier: %v", statErr)
			}
			// An interrupted creator leaves existing directories. A later opener must
			// complete the same barrier before treating them as established storage.
			registry, err = OpenRegistry(t.Context(), root)
			if err != nil {
				t.Fatal(err)
			}
			if err := registry.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
