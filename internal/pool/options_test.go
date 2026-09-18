package pool

import "testing"

func TestResponsiveness_EntrySeconds(t *testing.T) {
	if got := Responsive.entrySeconds(); got != 1 {
		t.Fatalf("Responsive.entrySeconds() = %d, want 1 (S8, doc 08 §8)", got)
	}
	if got := Quiet.entrySeconds(); got != 600 {
		t.Fatalf("Quiet.entrySeconds() = %d, want 600 (doc 08 §1's Quiet mode preset)", got)
	}
}

func TestOptions_Render(t *testing.T) {
	got := DefaultOptions().render()
	want := "moveonenospc=true,dropcacheonclose=true,minfreespace=50G,cache.files=partial,cache.entry=1,cache.attr=1,cache.negative_entry=1,cache.statfs=0"
	if got != want {
		t.Fatalf("DefaultOptions().render():\ngot:  %s\nwant: %s", got, want)
	}
}

func TestOptions_Render_FallsBackOnEmptyMinFreeSpace(t *testing.T) {
	got := Options{}.render()
	want := "moveonenospc=true,dropcacheonclose=true,minfreespace=50G,cache.files=partial,cache.entry=1,cache.attr=1,cache.negative_entry=1,cache.statfs=0"
	if got != want {
		t.Fatalf("Options{}.render():\ngot:  %s\nwant: %s (DefaultOptions()'s own minfreespace, not empty)", got, want)
	}
}
