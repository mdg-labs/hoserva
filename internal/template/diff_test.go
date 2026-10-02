package template

import (
	"strings"
	"testing"
)

func TestUnifiedDiff(t *testing.T) {
	cases := []struct {
		name     string
		from, to string
		want     string
	}{
		{name: "identical", from: "a\nb\n", to: "a\nb\n", want: ""},
		{
			name: "one changed line in the middle keeps three lines of context",
			from: "1\n2\n3\n4\n5\n6\n7\n8\n9\n",
			to:   "1\n2\n3\n4\nfive\n6\n7\n8\n9\n",
			want: "--- a\n+++ b\n@@ -2,7 +2,7 @@\n 2\n 3\n 4\n-5\n+five\n 6\n 7\n 8\n",
		},
		{
			name: "an insertion at the start",
			from: "b\nc\n",
			to:   "a\nb\nc\n",
			want: "--- a\n+++ b\n@@ -1,2 +1,3 @@\n+a\n b\n c\n",
		},
		{
			name: "everything removed",
			from: "a\nb\n",
			to:   "",
			want: "--- a\n+++ b\n@@ -1,2 +0,0 @@\n-a\n-b\n",
		},
		{
			name: "two distant changes make two hunks",
			from: "1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n11\n12\n13\n14\n",
			to:   "one\n2\n3\n4\n5\n6\n7\n8\n9\n10\n11\n12\n13\nfourteen\n",
			want: "--- a\n+++ b\n@@ -1,4 +1,4 @@\n-1\n+one\n 2\n 3\n 4\n@@ -11,4 +11,4 @@\n 11\n 12\n 13\n-14\n+fourteen\n",
		},
		{
			name: "a change in a repeated block is found by the common subsequence",
			from: "a\nx\nb\nx\nc\n",
			to:   "a\nb\nx\nc\n",
			want: "--- a\n+++ b\n@@ -1,5 +1,4 @@\n a\n-x\n b\n x\n c\n",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := UnifiedDiff("a", "b", c.from, c.to); got != c.want {
				t.Errorf("diff:\n%s\nwant:\n%s", got, c.want)
			}
		})
	}
}

func TestUnifiedDiffOfAHugeMiddleIsStillACorrectDiff(t *testing.T) {
	var from, to []string
	for i := 0; i < 3000; i++ {
		from = append(from, "old"+strings.Repeat("x", i%7))
		to = append(to, "new"+strings.Repeat("y", i%5))
	}
	got := UnifiedDiff("a", "b", strings.Join(from, "\n")+"\n", strings.Join(to, "\n")+"\n")
	if strings.Count(got, "\n-") < 3000 || strings.Count(got, "\n+") < 3000 {
		t.Fatalf("a diff of two entirely different texts must remove and add every line, got %d removals and %d additions", strings.Count(got, "\n-"), strings.Count(got, "\n+"))
	}
}
