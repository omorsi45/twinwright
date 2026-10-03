package shadow

import (
	"encoding/json"
	"sort"
	"strings"
)

// Comparison is a measurement of proposed vs observed actions.
//
// Matched, OnlyProposed and OnlyObserved answer "did the same thing happen".
// They cannot express the three ways a shadow run departs from a recorded one
// while still doing broadly the same work, which is where the interesting
// divergence lives:
//
//   - Arguments pairs actions that address the same resource with the same
//     operation and reports the fields that differ. Without it a refund of 500
//     against an observed refund of 5905 on one charge is two unrelated entries
//     in the only-lists, and the reader has to spot that they are the same
//     charge.
//   - Timing reports where the two streams disagree about order.
//   - Policy reports the proposed actions a principal policy would refuse. An
//     action a human performed is not evidence that this principal may perform
//     it, so a refusal stands even when the observed stream contains the same
//     action.
//
// Nothing here declares a winner. The observed stream is what happened, not what
// should have happened, and a divergence is a question for a reader.
type Comparison struct {
	Matched       []string             `json:"matched"`
	OnlyProposed  []string             `json:"only_proposed"`
	OnlyObserved  []string             `json:"only_observed"`
	ProposedCount int                  `json:"proposed_count"`
	ObservedCount int                  `json:"observed_count"`
	Arguments     []ArgumentDivergence `json:"argument_divergences"`
	Timing        TimingReport         `json:"timing"`
	Policy        PolicyReport         `json:"policy"`
}

// ArgumentDivergence is one action both streams took on one resource, with the
// arguments that differ.
type ArgumentDivergence struct {
	OperationID string            `json:"operation_id"`
	Resource    string            `json:"resource"`
	Fields      []FieldDivergence `json:"fields"`
}

// FieldDivergence carries both values. The presence flags exist because a field
// one side never sent is not a field set to null, and a bare null in a report
// cannot tell those apart.
type FieldDivergence struct {
	Field           string `json:"field"`
	Proposed        any    `json:"proposed"`
	Observed        any    `json:"observed"`
	ProposedPresent bool   `json:"proposed_present"`
	ObservedPresent bool   `json:"observed_present"`
}

// TimingReport carries the order comparison and the denominators behind it, so a
// reader can tell a clean comparison from one that had nothing to measure.
type TimingReport struct {
	PairedActions      int               `json:"paired_actions"`
	OrderDivergences   []OrderDivergence `json:"order_divergences"`
	ObservedTimestamps int               `json:"observed_timestamps"`
	ElapsedGap         string            `json:"elapsed_gap"`
}

// OrderDivergence is one pair of actions the two streams sequence differently.
type OrderDivergence struct {
	ProposedEarlier string `json:"proposed_earlier"`
	ObservedEarlier string `json:"observed_earlier"`
}

// PolicyReport is the authorization half of the comparison.
type PolicyReport struct {
	Evaluated bool `json:"evaluated"`
	// Absent says why there is no verdict. An empty refusal list with no
	// explanation reads exactly like a clean screen.
	Absent           string          `json:"absent,omitempty"`
	Principal        string          `json:"principal,omitempty"`
	Screened         int             `json:"screened"`
	Refused          []PolicyRefusal `json:"refused,omitempty"`
	InvalidArguments int             `json:"invalid_arguments"`
}

// PolicyRefusal is one proposed action the policy would not allow.
type PolicyRefusal struct {
	OperationID string         `json:"operation_id"`
	Arguments   map[string]any `json:"arguments,omitempty"`
	Resource    string         `json:"resource,omitempty"`
	Permission  string         `json:"permission,omitempty"`
	Reason      string         `json:"reason"`
	Call        int            `json:"call,omitempty"`
	// AlsoObserved reports that the observed stream contains the same operation
	// on the same resource. It does not soften the refusal; whoever performed it
	// was not this principal.
	AlsoObserved bool `json:"also_observed"`
}

// PolicyScreen is what a principal policy would have decided about a
// simulation's proposed actions. Simulate produces it because an accurate
// verdict needs the world the calls would have landed in.
type PolicyScreen struct {
	Principal        string          `json:"principal"`
	Screened         int             `json:"screened"`
	Refusals         []PolicyRefusal `json:"refusals,omitempty"`
	InvalidArguments int             `json:"invalid_arguments"`
}

// CompareOptions carries measurements Compare cannot make for itself.
type CompareOptions struct {
	// Policy is nil when no principal policy was configured. The report says so
	// rather than reporting zero refusals.
	Policy *PolicyScreen
}

// elapsedGapUnmeasured is stated in every report, because the alternative is a
// zero that reads like a measurement.
const elapsedGapUnmeasured = "not compared: a local simulation has no wall clock comparable to a recorded stream, so an elapsed gap has no proposed side"

const policyNotConfigured = "no principal policy was configured for this shadow run, so no authorization verdict was measured"

// Compare pairs actions and reports divergence. It does not declare a winner.
func Compare(proposed []ProposedAction, observed []Observation) (Comparison, error) {
	return CompareWith(proposed, observed, CompareOptions{})
}

// CompareWith is Compare with measurements made elsewhere folded in.
//
// Output order never depends on Go's randomised map iteration: every group is
// walked in sorted order and every list is sorted before it is returned. A
// shadow report is evidence, and evidence that reorders itself between two runs
// over identical input cannot be diffed or cited.
func CompareWith(proposed []ProposedAction, observed []Observation, options CompareOptions) (Comparison, error) {
	proposedEntries, err := proposedActionEntries(proposed)
	if err != nil {
		return Comparison{}, err
	}
	observedEntries, err := observationEntries(observed)
	if err != nil {
		return Comparison{}, err
	}

	// An identical action is the same action. Pairing those first means the
	// argument comparison only ever looks at what is left over, so a stream with
	// two refunds on one charge cannot report a divergence between the two that
	// already matched.
	identical, proposedLeft, observedLeft := pairOn(proposedEntries, observedEntries, func(e actionEntry) string {
		return e.op + "\x00" + e.canonical
	})
	// Same operation, same resource, different arguments. Pairing on the
	// operation alone would call a refund of one charge and a refund of another
	// the same action and report a divergence on charge_id, which reads as a
	// different amount rather than a different charge.
	diverged, onlyProposed, onlyObserved := pairOn(proposedLeft, observedLeft, func(e actionEntry) string {
		if e.resource == "" {
			return ""
		}
		return e.op + "\x00" + e.resource
	})

	out := Comparison{ProposedCount: len(proposed), ObservedCount: len(observed)}
	for _, p := range identical {
		out.Matched = append(out.Matched, p[0].op)
	}
	for _, e := range onlyProposed {
		out.OnlyProposed = append(out.OnlyProposed, e.op)
	}
	for _, e := range onlyObserved {
		out.OnlyObserved = append(out.OnlyObserved, e.op)
	}
	for _, p := range diverged {
		fields, err := divergingFields(p[0].arguments, p[1].arguments)
		if err != nil {
			return Comparison{}, err
		}
		out.Arguments = append(out.Arguments, ArgumentDivergence{OperationID: p[0].op, Resource: p[0].resource, Fields: fields})
	}

	out.Timing = timingOf(append(append([][2]actionEntry{}, identical...), diverged...), observed)
	out.Policy = policyOf(options.Policy, observedEntries)
	return out, nil
}

// actionEntry is one action from either stream, with everything the comparison
// keys on precomputed.
type actionEntry struct {
	op        string
	resource  string
	arguments map[string]any
	canonical string
	// index is the action's position in its own stream. It is used for ordering
	// only: identifying an action by its position would stop meaning anything
	// the moment a call site is added on one side.
	index int
}

func proposedActionEntries(proposed []ProposedAction) ([]actionEntry, error) {
	out := make([]actionEntry, 0, len(proposed))
	for i, p := range proposed {
		entry, err := newActionEntry(p.OperationID, p.Arguments, i)
		if err != nil {
			return nil, err
		}
		out = append(out, entry)
	}
	return out, nil
}

func observationEntries(observed []Observation) ([]actionEntry, error) {
	out := make([]actionEntry, 0, len(observed))
	for i, o := range observed {
		entry, err := newActionEntry(o.OperationID, o.Arguments, i)
		if err != nil {
			return nil, err
		}
		out = append(out, entry)
	}
	return out, nil
}

func newActionEntry(op string, arguments map[string]any, index int) (actionEntry, error) {
	// encoding/json sorts map keys, so this is canonical for a given set of
	// argument values.
	data, err := json.Marshal(arguments)
	if err != nil {
		return actionEntry{}, err
	}
	return actionEntry{op: op, resource: resourceOf(arguments), arguments: arguments, canonical: string(data), index: index}, nil
}

// resourceOf names the resource a call addresses, from the arguments that
// identify it rather than from the call's position or its operation alone. Both
// of those stop identifying anything as soon as a second call site appears.
//
// An argument identifies a resource when it is "id" or ends in "_id" and carries
// a non-empty string: every identifier in a compiled world is a string. A call
// with no such argument gets an empty resource and is never paired on resource,
// because there is nothing to establish that two of them address the same thing.
func resourceOf(arguments map[string]any) string {
	names := make([]string, 0, len(arguments))
	for name, value := range arguments {
		if name != "id" && !strings.HasSuffix(name, "_id") {
			continue
		}
		if text, ok := value.(string); !ok || strings.TrimSpace(text) == "" {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, name+"="+arguments[name].(string))
	}
	return strings.Join(parts, ",")
}

// label names an action for a reader: the operation, and the resource when the
// call identifies one.
func label(op, resource string) string {
	if resource == "" {
		return op
	}
	return op + "(" + resource + ")"
}

// pairOn groups both sides on key and pairs their members, returning the pairs
// and everything that stayed unpaired. An entry whose key is empty is never
// paired. Groups are walked in sorted key order and members in canonical then
// stream order, so the result is the same on every run.
func pairOn(left, right []actionEntry, key func(actionEntry) string) (pairs [][2]actionEntry, leftOver, rightOver []actionEntry) {
	group := func(entries []actionEntry) (map[string][]actionEntry, []actionEntry) {
		grouped := map[string][]actionEntry{}
		var unkeyed []actionEntry
		for _, e := range entries {
			k := key(e)
			if k == "" {
				unkeyed = append(unkeyed, e)
				continue
			}
			grouped[k] = append(grouped[k], e)
		}
		return grouped, unkeyed
	}
	leftGroups, leftUnkeyed := group(left)
	rightGroups, rightUnkeyed := group(right)
	leftOver, rightOver = leftUnkeyed, rightUnkeyed

	keys := make([]string, 0, len(leftGroups))
	for k := range leftGroups {
		keys = append(keys, k)
	}
	for k := range rightGroups {
		if _, ok := leftGroups[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	byCanonical := func(entries []actionEntry) {
		sort.Slice(entries, func(i, j int) bool {
			if entries[i].canonical != entries[j].canonical {
				return entries[i].canonical < entries[j].canonical
			}
			return entries[i].index < entries[j].index
		})
	}
	for _, k := range keys {
		l, r := leftGroups[k], rightGroups[k]
		byCanonical(l)
		byCanonical(r)
		n := len(l)
		if len(r) < n {
			n = len(r)
		}
		for i := 0; i < n; i++ {
			pairs = append(pairs, [2]actionEntry{l[i], r[i]})
		}
		leftOver = append(leftOver, l[n:]...)
		rightOver = append(rightOver, r[n:]...)
	}
	byOperation := func(entries []actionEntry) {
		sort.Slice(entries, func(i, j int) bool {
			if entries[i].op != entries[j].op {
				return entries[i].op < entries[j].op
			}
			if entries[i].canonical != entries[j].canonical {
				return entries[i].canonical < entries[j].canonical
			}
			return entries[i].index < entries[j].index
		})
	}
	byOperation(leftOver)
	byOperation(rightOver)
	return pairs, leftOver, rightOver
}

// divergingFields reports the fields two paired actions disagree on, including
// the ones only one side sent.
func divergingFields(proposed, observed map[string]any) ([]FieldDivergence, error) {
	names := make([]string, 0, len(proposed))
	for name := range proposed {
		names = append(names, name)
	}
	for name := range observed {
		if _, ok := proposed[name]; !ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	var out []FieldDivergence
	for _, name := range names {
		proposedValue, proposedPresent := proposed[name]
		observedValue, observedPresent := observed[name]
		if proposedPresent && observedPresent {
			// Compared as encoded values: an amount read from the world arrives
			// as an int64 and the same amount from a JSONL source as a
			// json.Number, and those are the same observation.
			same, err := sameEncoded(proposedValue, observedValue)
			if err != nil {
				return nil, err
			}
			if same {
				continue
			}
		}
		out = append(out, FieldDivergence{
			Field: name, Proposed: proposedValue, Observed: observedValue,
			ProposedPresent: proposedPresent, ObservedPresent: observedPresent,
		})
	}
	return out, nil
}

func sameEncoded(left, right any) (bool, error) {
	l, err := json.Marshal(left)
	if err != nil {
		return false, err
	}
	r, err := json.Marshal(right)
	if err != nil {
		return false, err
	}
	return string(l) == string(r), nil
}

// timingOf reports where the two streams sequence the same actions differently.
//
// Order is the timing property both sides actually carry. Elapsed time is not:
// the proposed side comes from a local simulation whose wall clock has nothing
// to do with how long a human took, so a gap comparison would be noise wearing a
// measurement's clothes. The report says that instead of printing a zero, and
// carries how many observations had a timestamp at all.
func timingOf(pairs [][2]actionEntry, observed []Observation) TimingReport {
	report := TimingReport{PairedActions: len(pairs), ElapsedGap: elapsedGapUnmeasured}
	for _, o := range observed {
		if strings.TrimSpace(o.At) != "" {
			report.ObservedTimestamps++
		}
	}
	ordered := append([][2]actionEntry{}, pairs...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i][0].index < ordered[j][0].index })
	for i := 0; i < len(ordered); i++ {
		for j := i + 1; j < len(ordered); j++ {
			if ordered[j][1].index < ordered[i][1].index {
				report.OrderDivergences = append(report.OrderDivergences, OrderDivergence{
					ProposedEarlier: label(ordered[i][0].op, ordered[i][0].resource),
					ObservedEarlier: label(ordered[j][0].op, ordered[j][0].resource),
				})
			}
		}
	}
	return report
}

func policyOf(screen *PolicyScreen, observed []actionEntry) PolicyReport {
	if screen == nil {
		return PolicyReport{Absent: policyNotConfigured}
	}
	report := PolicyReport{
		Evaluated: true, Principal: screen.Principal,
		Screened: screen.Screened, InvalidArguments: screen.InvalidArguments,
	}
	for _, refusal := range screen.Refusals {
		refusal.Resource = resourceOf(refusal.Arguments)
		refusal.AlsoObserved = observedContains(observed, refusal)
		report.Refused = append(report.Refused, refusal)
	}
	return report
}

// observedContains reports whether the observed stream holds the refused action:
// the same operation on the same resource, or a byte-identical call when the
// action identifies no resource.
func observedContains(observed []actionEntry, refusal PolicyRefusal) bool {
	candidate, err := newActionEntry(refusal.OperationID, refusal.Arguments, 0)
	if err != nil {
		return false
	}
	for _, o := range observed {
		if o.op != candidate.op {
			continue
		}
		if candidate.resource != "" && o.resource == candidate.resource {
			return true
		}
		if candidate.resource == "" && o.canonical == candidate.canonical {
			return true
		}
	}
	return false
}
