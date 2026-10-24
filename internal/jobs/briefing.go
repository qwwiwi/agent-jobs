package jobs

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// Bounds of a briefing block (F8): questions per round put to the owner at once, and questions of the
// optional adversarial pass for a large feature.
const (
	MaxBriefingRoundQuestions = 4
	MaxBriefingAdversarial    = 7
)

// BriefingKinds are the only kinds of choice a briefing puts to the owner: cost or dependence on a
// vendor, what the owner will see, decisions expensive to undo, business rules. What can be found out
// is the job's to find out, never a question.
var BriefingKinds = []string{"cost_or_vendor", "owner_visible", "costly_to_undo", "business_rule"}

// AssumptionPrefix starts every decided entry: the owner reads it as the job's assumption, not as a
// decision the owner made.
const AssumptionPrefix = "ASSUMPTION:"

// briefingSecretRe catches a question that asks the owner for a secret: the words password, passphrase,
// token, secret, credential, key (api key, ssh key, ...) and their Russian counterparts, as whole words
// (any Unicode letter or digit next to them makes another word). A question never needs them: a
// credential is set up by the owner out of band, never typed into a reply.
var briefingSecretRe = regexp.MustCompile(`(?i)(?:^|[^\p{L}\p{N}_])(?:passwords?|passwds?|passphrases?|pass\s*words?|tokens?|` +
	`secrets?|credentials?|api[\s_-]*keys?|apikeys?|keys?|парол\p{L}*|токен\p{L}*|ключ\p{L}*|секрет\p{L}*)(?:$|[^\p{L}\p{N}_])`)

// BriefingOutput is the agent-jobs/briefing/v1 block of a briefing job's report.
type BriefingOutput struct {
	Schema      string                   `json:"schema"`
	Checklist   GateChecklistRef         `json:"checklist"`
	Rounds      []store.BriefingRound    `json:"rounds"`
	Decided     []store.BriefingDecided  `json:"decided"`
	Adversarial []store.BriefingQuestion `json:"adversarial,omitempty"`
}

// ValidateBriefing parses and validates a briefing job's report against its input: exactly one
// agent-jobs/briefing/v1 block, no unknown fields, the input's checklist, rounds and decided present,
// every round of 1 to 4 questions, at most 7 adversarial questions, every question numbered once (n
// unique across rounds and adversarial), of a BriefingKinds kind, with q, recommended and why, closing
// at least one row of the checklist and asking for no secret; every decided entry an ASSUMPTION closing
// at least one row. It returns the block with its texts redacted.
func ValidateBriefing(report string, g *store.GateInput) (*BriefingOutput, error) {
	text, err := schemaBlock(report, BriefingSchema)
	if err != nil {
		return nil, err
	}
	var o BriefingOutput
	if err := decodeStrict(text, &o); err != nil {
		return nil, fmt.Errorf("%s block: %w", BriefingSchema, err)
	}
	if err := checkRef(o.Checklist, g); err != nil {
		return nil, err
	}
	if err := gateList("rounds", o.Rounds); err != nil {
		return nil, err
	}
	if err := gateList("decided", o.Decided); err != nil {
		return nil, err
	}
	rows := gateRowIDs(g)
	closes := func(what string, ids []string) error {
		if len(ids) == 0 {
			return fmt.Errorf("%s closes no row (it must name the checklist rows it settles)", what)
		}
		if len(ids) > maxGateItems {
			return fmt.Errorf("%s closes %d rows (at most %d)", what, len(ids), maxGateItems)
		}
		for k, id := range ids {
			if !slices.Contains(rows, id) {
				return fmt.Errorf("%s closes %q, not a row of the checklist", what, id)
			}
			if slices.Contains(ids[:k], id) {
				return fmt.Errorf("%s closes row %s twice", what, id)
			}
		}
		return nil
	}
	var numbers []int
	question := func(what string, q *store.BriefingQuestion) error {
		if q.N < 1 {
			return fmt.Errorf("%s: n %d (questions are numbered from 1)", what, q.N)
		}
		if slices.Contains(numbers, q.N) {
			return fmt.Errorf("%s: n %d is used twice (numbers are unique across rounds and adversarial)", what, q.N)
		}
		numbers = append(numbers, q.N)
		what = fmt.Sprintf("%s (n %d)", what, q.N)
		if err := closes(what, q.Closes); err != nil {
			return err
		}
		if !slices.Contains(BriefingKinds, q.Kind) {
			return fmt.Errorf("%s: kind %q must be one of %s (anything else the job finds out itself)", what, q.Kind,
				strings.Join(BriefingKinds, "|"))
		}
		var err error
		for _, f := range []struct {
			name string
			v    *string
		}{{"q", &q.Q}, {"recommended", &q.Recommended}, {"why", &q.Why}} {
			// Checked before redaction: a masked value must not hide what the question asks for.
			if f.name != "why" && briefingSecretRe.MatchString(*f.v) {
				return fmt.Errorf("%s: %s asks for a secret (password, token, key and the like are never asked of the owner)", what, f.name)
			}
			if *f.v, err = gateText(what+" "+f.name, *f.v, true); err != nil {
				return err
			}
		}
		return nil
	}
	for i := range o.Rounds {
		r := &o.Rounds[i]
		what := fmt.Sprintf("round %d", i+1)
		if len(r.Questions) == 0 {
			return nil, fmt.Errorf("%s has no questions (leave an empty round out)", what)
		}
		if len(r.Questions) > MaxBriefingRoundQuestions {
			return nil, fmt.Errorf("%s has %d questions (at most %d per round)", what, len(r.Questions), MaxBriefingRoundQuestions)
		}
		for k := range r.Questions {
			if err := question(fmt.Sprintf("%s question %d", what, k+1), &r.Questions[k]); err != nil {
				return nil, err
			}
		}
	}
	if len(o.Adversarial) > MaxBriefingAdversarial {
		return nil, fmt.Errorf("adversarial has %d questions (at most %d)", len(o.Adversarial), MaxBriefingAdversarial)
	}
	for k := range o.Adversarial {
		if err := question(fmt.Sprintf("adversarial question %d", k+1), &o.Adversarial[k]); err != nil {
			return nil, err
		}
	}
	o.Adversarial = nonNilSlice(o.Adversarial)
	for i := range o.Decided {
		d := &o.Decided[i]
		what := fmt.Sprintf("decided %d", i+1)
		if d.Text, err = gateText(what+" text", d.Text, true); err != nil {
			return nil, err
		}
		if !strings.HasPrefix(d.Text, AssumptionPrefix) {
			return nil, fmt.Errorf("%s: text must start with %q (the owner reads it as the job's assumption)", what, AssumptionPrefix)
		}
		if err := closes(what, d.Closes); err != nil {
			return nil, err
		}
	}
	return &o, nil
}

// BriefingView is briefing_get's result: the briefing for the checklist's current version and hash
// (Current true), else the latest one of the checklist id (an older draft).
type BriefingView struct {
	Current bool                  `json:"current"`
	Result  *store.BriefingResult `json:"result"`
}

// BriefingOf returns the briefing result of checklist id in repo main for version and hash, else the
// latest one of the id (nil = none).
func (m *Manager) BriefingOf(main, id string, version int, hash string) (*BriefingView, error) {
	r, err := m.Store.BriefingFor(main, id, version, hash)
	if err != nil {
		return nil, err
	}
	if r != nil {
		return &BriefingView{Current: true, Result: r}, nil
	}
	if r, err = m.Store.LatestBriefing(main, id); err != nil || r == nil {
		return nil, err
	}
	return &BriefingView{Result: r}, nil
}
