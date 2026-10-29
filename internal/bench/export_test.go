package bench

// FoldTrials exposes the pure fold to the external test package. That package
// can import internal/gate, which this package's own tests cannot (gate imports
// bench), so a test can fold trials and gate the folded result with the real
// comparison.
var FoldTrials = foldTrials
