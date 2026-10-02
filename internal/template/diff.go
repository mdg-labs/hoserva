package template

import (
	"fmt"
	"strings"
)

const (
	diffContext = 3
	// maxDiffCells bounds the table the line comparison fills; a pair of
	// texts whose changed middle is larger is reported as that middle
	// removed and re-added, which is a correct diff if not a minimal one.
	maxDiffCells = 4_000_000
)

type diffOp struct {
	kind byte
	line string
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// UnifiedDiff is the unified diff turning from into to, three lines of
// context per hunk, with fromName and toName on its two header lines. It is
// empty when the two texts have the same lines.
func UnifiedDiff(fromName, toName, from, to string) string {
	a, b := splitLines(from), splitLines(to)
	prefix := 0
	for prefix < len(a) && prefix < len(b) && a[prefix] == b[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(a)-prefix && suffix < len(b)-prefix && a[len(a)-1-suffix] == b[len(b)-1-suffix] {
		suffix++
	}
	ops := make([]diffOp, 0, len(a)+len(b))
	for _, l := range a[:prefix] {
		ops = append(ops, diffOp{' ', l})
	}
	ops = append(ops, diffMiddle(a[prefix:len(a)-suffix], b[prefix:len(b)-suffix])...)
	for _, l := range a[len(a)-suffix:] {
		ops = append(ops, diffOp{' ', l})
	}

	var sb strings.Builder
	aBefore, bBefore := 0, 0
	for i := 0; i < len(ops); {
		for i < len(ops) && ops[i].kind == ' ' {
			aBefore++
			bBefore++
			i++
		}
		if i == len(ops) {
			break
		}
		start := i - diffContext
		if start < 0 {
			start = 0
		}
		// The hunk starts with the context before the change, which the
		// skipped equal lines above already counted as before it.
		skipped := i - start
		aBefore -= skipped
		bBefore -= skipped
		lastChange := i
		for k := i; k < len(ops); k++ {
			if ops[k].kind != ' ' {
				lastChange = k
			} else if k-lastChange > 2*diffContext {
				break
			}
		}
		end := lastChange + diffContext + 1
		if end > len(ops) {
			end = len(ops)
		}
		aLen, bLen := 0, 0
		for _, op := range ops[start:end] {
			if op.kind != '+' {
				aLen++
			}
			if op.kind != '-' {
				bLen++
			}
		}
		if sb.Len() == 0 {
			fmt.Fprintf(&sb, "--- %s\n+++ %s\n", fromName, toName)
		}
		aStart, bStart := aBefore+1, bBefore+1
		if aLen == 0 {
			aStart = aBefore
		}
		if bLen == 0 {
			bStart = bBefore
		}
		fmt.Fprintf(&sb, "@@ -%d,%d +%d,%d @@\n", aStart, aLen, bStart, bLen)
		for _, op := range ops[start:end] {
			sb.WriteByte(op.kind)
			sb.WriteString(op.line)
			sb.WriteByte('\n')
		}
		aBefore += aLen
		bBefore += bLen
		i = end
	}
	return sb.String()
}

// diffMiddle compares the lines left between a common prefix and suffix.
func diffMiddle(a, b []string) []diffOp {
	var ops []diffOp
	if len(a) == 0 || len(b) == 0 || len(a)*len(b) > maxDiffCells {
		for _, l := range a {
			ops = append(ops, diffOp{'-', l})
		}
		for _, l := range b {
			ops = append(ops, diffOp{'+', l})
		}
		return ops
	}
	// lcs[i][j] is the length of the longest common subsequence of a[i:]
	// and b[j:].
	lcs := make([][]int32, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int32, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			switch {
			case a[i] == b[j]:
				lcs[i][j] = lcs[i+1][j+1] + 1
			case lcs[i+1][j] >= lcs[i][j+1]:
				lcs[i][j] = lcs[i+1][j]
			default:
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			ops = append(ops, diffOp{' ', a[i]})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			ops = append(ops, diffOp{'-', a[i]})
			i++
		default:
			ops = append(ops, diffOp{'+', b[j]})
			j++
		}
	}
	for ; i < len(a); i++ {
		ops = append(ops, diffOp{'-', a[i]})
	}
	for ; j < len(b); j++ {
		ops = append(ops, diffOp{'+', b[j]})
	}
	return ops
}
