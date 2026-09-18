package pool

import "testing"

func TestCreatePolicy_Label(t *testing.T) {
	cases := map[CreatePolicy]string{
		KeepFoldersTogether: "Keep folders together",
		BalanceAcrossDisks:  "Balance across disks",
		QuietDisks:          "Quiet disks",
		FillDisksInOrder:    "Fill disks in order",
	}
	for policy, want := range cases {
		if got := policy.Label(); got != want {
			t.Fatalf("%s.Label(): got %q, want %q", policy, got, want)
		}
	}
}

func TestDefaultCreatePolicy(t *testing.T) {
	if DefaultCreatePolicy != KeepFoldersTogether {
		t.Fatalf("DefaultCreatePolicy = %s, want %s (Q11: default for new shares)", DefaultCreatePolicy, KeepFoldersTogether)
	}
}
