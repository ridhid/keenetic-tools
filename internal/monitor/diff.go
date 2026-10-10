package monitor

import (
	"context"
	"fmt"
	"strings"
)

func bgctx() context.Context { return context.Background() }

// unifiedDiff is `diff -u` for small inputs (snapshots are a few hundred
// lines): LCS by lines, hunks with 3 lines of context. "" means equal.
func unifiedDiff(nameA, nameB string, a, b []string) string {
	const context = 3
	if len(a)*len(b) > 4_000_000 {
		// Too big for the table on a router: show everything as changed.
		ops := make([]op, 0, len(a)+len(b))
		for _, l := range a {
			ops = append(ops, op{'-', l})
		}
		for _, l := range b {
			ops = append(ops, op{'+', l})
		}
		return render(nameA, nameB, ops, context)
	}
	// lcs[i][j] = LCS length of a[i:] and b[j:].
	lcs := make([][]int32, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int32, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var ops []op
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		switch {
		case i < len(a) && j < len(b) && a[i] == b[j]:
			ops = append(ops, op{' ', a[i]})
			i++
			j++
		case j < len(b) && (i == len(a) || lcs[i][j+1] > lcs[i+1][j]):
			ops = append(ops, op{'+', b[j]})
			j++
		default:
			ops = append(ops, op{'-', a[i]})
			i++
		}
	}
	return render(nameA, nameB, ops, context)
}

type op struct {
	kind byte
	text string
}

func render(nameA, nameB string, ops []op, context int) string {
	changed := false
	for _, o := range ops {
		if o.kind != ' ' {
			changed = true
			break
		}
	}
	if !changed {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "--- %s\n+++ %s\n", nameA, nameB)
	// Line numbers before each op.
	na, nb := make([]int, len(ops)+1), make([]int, len(ops)+1)
	for k, o := range ops {
		na[k+1], nb[k+1] = na[k], nb[k]
		if o.kind != '+' {
			na[k+1]++
		}
		if o.kind != '-' {
			nb[k+1]++
		}
	}
	for k := 0; k < len(ops); {
		if ops[k].kind == ' ' {
			k++
			continue
		}
		start := max(k-context, 0)
		end := k
		// Extend while the next change is within 2*context lines.
		for end < len(ops) {
			if ops[end].kind != ' ' {
				end++
				continue
			}
			run := end
			for run < len(ops) && ops[run].kind == ' ' {
				run++
			}
			if run == len(ops) || run-end > 2*context {
				end = min(end+context, len(ops))
				break
			}
			end = run
		}
		la, lb := na[end]-na[start], nb[end]-nb[start]
		fmt.Fprintf(&b, "@@ -%s +%s @@\n", hunkRange(na[start], la), hunkRange(nb[start], lb))
		for _, o := range ops[start:end] {
			b.WriteByte(o.kind)
			b.WriteString(o.text)
			b.WriteByte('\n')
		}
		k = end
	}
	return b.String()
}

func hunkRange(start, n int) string {
	if n == 0 {
		return fmt.Sprintf("%d,0", start)
	}
	if n == 1 {
		return fmt.Sprint(start + 1)
	}
	return fmt.Sprintf("%d,%d", start+1, n)
}
