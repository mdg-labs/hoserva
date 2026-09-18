package pool

import (
	"errors"
	"testing"
)

func TestValidateShareName(t *testing.T) {
	valid := []string{"movies", "Media_1", "tv-shows", "a1"}
	for _, name := range valid {
		if err := ValidateShareName(name); err != nil {
			t.Fatalf("ValidateShareName(%q): got %v, want nil", name, err)
		}
	}

	invalid := []string{"", "../etc", "a/b", ".hidden", "share name", "share/../x"}
	for _, name := range invalid {
		if err := ValidateShareName(name); !errors.Is(err, ErrInvalidShareName) {
			t.Fatalf("ValidateShareName(%q): got %v, want ErrInvalidShareName", name, err)
		}
	}
}
