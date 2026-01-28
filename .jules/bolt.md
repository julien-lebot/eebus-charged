## 2025-05-27 - Inefficient String Concatenation in Go
**Learning:** Found O(n^2) string concatenation in a loop (using `+=` with immutable strings). Replacing with `strings.ReplaceAll` improved performance by 4.4x and reduced allocs from 63 to 1.
**Action:** Always check for `+=` string concatenation inside loops, even for short strings. Use `strings.Builder` or `strings.ReplaceAll` instead.
