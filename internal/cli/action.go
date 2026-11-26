package cli

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"

	"github.com/urfave/cli/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/apierr"
)

// Flag names of the actions.
const (
	flagYes              = "yes"
	flagDetach           = "detach"
	flagRequestID        = "request-id"
	flagDryRun           = "dry-run"
	flagConfirmIsolation = "confirm-isolation"
	flagConfirmNotMaxLSN = "confirm-not-max-lsn"
	flagMaxDiscard       = "max-discard-bytes"
)

const (
	// maxRounds bounds the dry-runs made to settle the confirmations: facts
	// that change on every one of them are not something to confirm.
	maxRounds = 5
	// maxStartAttempts bounds the attempts to start an action, and to confirm
	// it again after the facts changed under the typed name.
	maxStartAttempts = 3
)

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// invoke calls the RPC of an action. A nil confirmation sends none.
type invoke func(ctx context.Context, validateOnly bool, conf *dcctlv1.Confirmation, requestID string) (*dcctlv1.Operation, error)

// plan is what an action command knows after its arguments are read.
type plan struct {
	invoke invoke
	// describe says what the action does, for the question the operator
	// answers.
	describe string
	// guard is the name the operator types to start the action; without it
	// the target of the plan is, else fallbackGuard.
	guard         string
	fallbackGuard string
	// resuming marks resume: it goes on in forced mode only with the isolation
	// confirmed, so that is sent only when the checks show it is needed.
	resuming bool
	// abandoning marks abandon, which succeeds with the ABANDONED state.
	abandoning bool
	// idempotent is set when the request carries a request_id, so a call that
	// failed on the way can be repeated.
	idempotent bool
}

type actionSpec struct {
	name      string
	usage     string
	argsUsage string
	flags     []cli.Flag
	before    cli.BeforeFunc
	plan      func(ctx context.Context, c *cli.Context, s *session) (plan, error)
}

func actionFlags() []cli.Flag {
	return []cli.Flag{
		&cli.BoolFlag{Name: flagDryRun, Usage: "only show the checks, the plan and the confirmations it needs"},
		&cli.BoolFlag{Name: flagYes, Usage: "do not check first and do not ask to type the name: " +
			"the confirmations are only those of the flags"},
		&cli.BoolFlag{Name: flagDetach, Usage: "print the operation and return, do not follow it"},
		&cli.StringFlag{Name: flagRequestID, Usage: "UUID that makes a repeat of the command return the operation it started (default: a new one)"},
	}
}

func isolationFlag() cli.Flag {
	return &cli.BoolFlag{Name: flagConfirmIsolation, Usage: "the old primary is isolated"}
}

func notMaxLSNFlag() cli.Flag {
	return &cli.BoolFlag{Name: flagConfirmNotMaxLSN, Usage: "accept a target that does not have the largest received LSN"}
}

func maxDiscardFlag() cli.Flag {
	return &cli.Int64Flag{Name: flagMaxDiscard, Usage: "accept discarding up to this many bytes of data", Value: 0}
}

func (o Options) action(spec actionSpec) *cli.Command {
	flags := append(connFlags(), actionFlags()...)

	return leaf(&cli.Command{
		Name:      spec.name,
		Usage:     spec.usage,
		ArgsUsage: spec.argsUsage,
		Flags:     append(flags, spec.flags...),
		Before:    spec.before,
		Action: o.run(func(ctx context.Context, c *cli.Context, s *session) error {
			p, err := spec.plan(ctx, c, s)
			if err != nil {
				return err
			}

			return s.runAction(ctx, c, p)
		}),
	})
}

// newRequestID returns a random UUID v4.
func newRequestID() string {
	var b [16]byte

	_, _ = rand.Read(b[:])

	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80

	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func requestIDOf(c *cli.Context) (string, error) {
	id := c.String(flagRequestID)
	if id == "" {
		return newRequestID(), nil
	}

	if !uuidPattern.MatchString(id) {
		return "", usageErrorf("--%s %q is not a UUID", flagRequestID, id)
	}

	return strings.ToLower(id), nil
}

func offerOf(c *cli.Context) (offer, error) {
	o := offer{isolation: c.Bool(flagConfirmIsolation), notMaxLSN: c.Bool(flagConfirmNotMaxLSN)}

	if c.IsSet(flagMaxDiscard) {
		n := c.Int64(flagMaxDiscard)
		if n < 0 {
			return offer{}, usageErrorf("--%s must not be negative", flagMaxDiscard)
		}

		o.discard = &n
	}

	return o, nil
}

// runAction runs an action the way the operator is protected: a dry-run
// shows the checks, the plan and the confirmations it needs, the operator
// confirms and types the name, and the action starts with one request ID,
// whatever happens to the call. Then the operation is followed until it is
// final.
//
// With --yes there is no dry-run and no question: the action starts with the
// confirmations of the flags, and a refusal that asks for more fails with
// the facts. With --dry-run nothing starts.
func (s *session) runAction(ctx context.Context, c *cli.Context, p plan) error {
	err := s.act(ctx, c, p)
	if err != nil && ctx.Err() != nil && ExitCode(err) == ExitFailed {
		err = &Error{Code: ExitCanceled, Err: err, JSON: s.asJSON}
	}

	return err
}

func (s *session) act(ctx context.Context, c *cli.Context, p plan) error {
	reqID, err := requestIDOf(c)
	if err != nil {
		return err
	}

	off, err := offerOf(c)
	if err != nil {
		return err
	}

	dryRun := c.Bool(flagDryRun)

	if c.Bool(flagYes) && !dryRun {
		op, err := s.start(ctx, p, off.confirmation(), reqID)
		if err != nil {
			return err
		}

		return s.finish(ctx, c, p, op)
	}

	dec := make(decisions)

	for range maxStartAttempts {
		preview, err := s.settle(ctx, p, off, dec, reqID, dryRun)
		if dryRun {
			return s.showDryRun(preview, err)
		}

		if err != nil {
			return err
		}

		s.showPlan(preview, dec)

		if err := s.typeName(ctx, p, preview); err != nil {
			return err
		}

		op, err := s.start(ctx, p, dec.confirmation(), reqID)
		if apierr.ReasonOf(err) == apierr.ReasonConfirmationRequired {
			_, _ = fmt.Fprintln(s.errOut, "The facts changed after the check: checking again.")

			continue
		}

		if err != nil {
			return err
		}

		return s.finish(ctx, c, p, op)
	}

	return errors.New("the facts keep changing: nothing was started")
}

// settle makes dry-runs until the confirmations in dec cover what the checks
// ask for, asking the operator for each one the flags do not give, and
// returns the plan of the last dry-run. It returns the plan, if there is
// one, with an error that stops the action as well.
func (s *session) settle(ctx context.Context, p plan, off offer, dec decisions, reqID string, dryRun bool) (*dcctlv1.Operation, error) {
	for range maxRounds {
		op, err := call(ctx, s, func(ctx context.Context) (*dcctlv1.Operation, error) {
			return p.invoke(ctx, true, dec.confirmation(), reqID)
		})

		refused := apierr.ReasonOf(err) == apierr.ReasonConfirmationRequired

		switch {
		case err == nil:
		case refused:
			st, _ := statusOf(err)

			pf, _ := detailsOf(st)
			if pf == nil {
				return nil, err
			}

			op = &dcctlv1.Operation{Checks: pf.GetChecks(), Steps: pf.GetSteps(), Mode: pf.GetMode()}
		default:
			return nil, err
		}

		needs, nerr := needsOf(op.GetChecks(), p.resuming)
		if nerr != nil {
			return op, nerr
		}

		dec.keep(needs)

		unmet := dec.unmet(needs)

		switch {
		case len(unmet) == 0 && !refused:
			return op, nil
		case len(unmet) == 0:
			// The server asks for something the checks do not show.
			return op, err
		}

		if p.resuming {
			unmet = s.showIsolationFacts(ctx, p, dec, reqID, unmet)
		}

		for _, n := range unmet {
			d, ok, aerr := off.answer(n)
			if aerr != nil {
				return op, aerr
			}

			switch {
			case ok:
			case dryRun:
				return op, confirmationRequired(unmet)
			default:
				if d, aerr = s.ask(ctx, n); aerr != nil {
					return op, aerr
				}
			}

			d.facts = n.facts
			if n.kind == confMaxDiscard && !d.flag {
				d.amount = n.amount
			}

			dec[n.kind] = d
		}
	}

	return nil, errors.New("the facts keep changing: nothing was started")
}

// confirmationRequired is the error of a dry-run whose confirmations the
// flags do not give.
func confirmationRequired(unmet []need) error {
	var b strings.Builder

	b.WriteString("the action needs confirmations the flags do not give:")

	for _, n := range unmet {
		fmt.Fprintf(&b, "\n  %s", confirmationFlag(n.kind))

		for _, c := range n.checks {
			fmt.Fprintf(&b, "\n    %s: %s", c.GetId(), c.GetDetail())

			if facts, err := marshalJSON(c.GetFacts(), false); err == nil && c.GetFacts() != nil {
				fmt.Fprintf(&b, "\n      facts: %s", facts)
			}
		}
	}

	return errors.New(b.String())
}

// ask asks the operator for the confirmation n, with what the checks show.
func (s *session) ask(ctx context.Context, n need) (decision, error) {
	w := s.errOut

	_, _ = fmt.Fprintln(w)

	for _, c := range n.checks {
		_, _ = fmt.Fprintf(w, "%s %s: %s\n", c.GetId(), dash(shortName(c.GetDatacenter())), c.GetDetail())

		if c.GetFacts() == nil {
			continue
		}

		if b, err := marshalJSON(c.GetFacts(), false); err == nil {
			_, _ = fmt.Fprintf(w, "  facts: %s\n", b)
		}
	}

	var question string

	switch n.kind {
	case confIsolation:
		question = "Confirm that the old primary is isolated"
	case confNotMaxLSN:
		question = "Confirm the target although it does not have the largest received LSN"
	case confMaxDiscard:
		if n.amount < 0 {
			return decision{}, errors.New("the server asks to accept discarded data but does not say how much: " +
				"pass --" + flagMaxDiscard + " with the amount you accept")
		}

		question = fmt.Sprintf("Accept discarding up to %d bytes of data", n.amount)
	}

	_, _ = fmt.Fprintf(w, "%s? [y/N] ", question)

	line, err := s.readLine(ctx)
	if err != nil {
		return decision{}, err
	}

	if a := strings.ToLower(line); a != "y" && a != "yes" {
		return decision{}, errors.New("not confirmed: nothing was started")
	}

	return decision{}, nil
}

// typeName asks the operator to type the name of what the action is about.
func (s *session) typeName(ctx context.Context, p plan, preview *dcctlv1.Operation) error {
	word := p.guard
	if word == "" {
		word = shortName(preview.GetTarget())
	}

	if word == "" {
		word = p.fallbackGuard
	}

	_, _ = fmt.Fprintf(s.errOut, "\n%s.\nType %q to continue: ", p.describe, word)

	line, err := s.readLine(ctx)
	if err != nil {
		return err
	}

	if line != word {
		return errors.New("the typed name does not match: nothing was started")
	}

	return nil
}

// readLine reads one line from the input of the command. It returns when the
// command is canceled, though the read itself cannot be: the process ends
// right after, and the reader goes with it.
func (s *session) readLine(ctx context.Context) (string, error) {
	type read struct {
		line string
		err  error
	}

	done := make(chan read, 1)

	go func() {
		line, err := s.in.ReadString('\n')
		done <- read{line, err}
	}()

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case r := <-done:
		if r.err != nil && (!errors.Is(r.err, io.EOF) || r.line == "") {
			return "", errors.New("no answer on the input: run the command in a terminal, or pass --" + flagYes)
		}

		return strings.TrimSpace(r.line), nil
	}
}

// planWriter is where the plan is shown: the output, or stderr when stdout
// carries JSON.
func (s *session) planWriter() io.Writer {
	if s.asJSON {
		return s.errOut
	}

	return s.out
}

func (s *session) showPlan(preview *dcctlv1.Operation, dec decisions) {
	w := s.planWriter()

	_, _ = fmt.Fprintln(w, "Nothing is changed yet. The plan:")

	printOperation(w, preview)

	if c := dec.confirmation(); c != nil {
		_, _ = fmt.Fprintf(w, "\nConfirmed: %s\n", confirmedText(c))
	}
}

func confirmedText(c *dcctlv1.Confirmation) string {
	var parts []string

	if c.GetIsolation() {
		parts = append(parts, "the old primary is isolated")
	}

	if c.GetNotMaxLsn() {
		parts = append(parts, "the target does not have the largest LSN")
	}

	if c.GetMaxDiscardBytes() > 0 {
		parts = append(parts, fmt.Sprintf("discarding up to %d bytes", c.GetMaxDiscardBytes()))
	}

	return strings.Join(parts, ", ")
}

// showDryRun prints the result of --dry-run and returns its error.
func (s *session) showDryRun(preview *dcctlv1.Operation, err error) error {
	if preview != nil {
		if s.asJSON {
			if jerr := s.printJSON(preview); jerr != nil {
				return jerr
			}
		} else {
			_, _ = fmt.Fprintln(s.out, "Dry run, nothing was changed. The plan:")
			printOperation(s.out, preview)
		}
	}

	return err
}

// start runs the action. A call that failed on the way is repeated with the
// same request ID, which makes a repeat return the operation that started
// instead of starting another.
func (s *session) start(ctx context.Context, p plan, conf *dcctlv1.Confirmation, reqID string) (*dcctlv1.Operation, error) {
	for attempt := 1; ; attempt++ {
		op, err := call(ctx, s, func(ctx context.Context) (*dcctlv1.Operation, error) {
			return p.invoke(ctx, false, conf, reqID)
		})
		if err == nil {
			return op, nil
		}

		// A call that was canceled or timed out may have reached the server.
		unknown := ctx.Err() != nil || retryable(err) || status.Code(err) == codes.Canceled
		if !unknown {
			return nil, err
		}

		if !p.idempotent {
			return nil, withHint(err, "the action may have been done: look at the operations with \"webitel-dcctl ops\" before you repeat it")
		}

		if ctx.Err() != nil || !retryable(err) || attempt >= maxStartAttempts {
			return nil, withHint(err, "the action may have started: repeat the command with --%s %s, "+
				"which returns the operation it started instead of starting another", flagRequestID, reqID)
		}

		if err := sleep(ctx, s.o.RetryDelay); err != nil {
			return nil, err
		}
	}
}

// finish follows the operation the server returned, or prints it for
// --detach.
func (s *session) finish(ctx context.Context, c *cli.Context, p plan, op *dcctlv1.Operation) error {
	if !c.Bool(flagDetach) {
		return s.follow(ctx, op, p.abandoning)
	}

	if err := s.printFinal(op); err != nil {
		return err
	}

	if !s.asJSON && op.GetState() == dcctlv1.Operation_RUNNING {
		_, _ = fmt.Fprintf(s.out, "\nFollow it with: webitel-dcctl watch %s\n", shortName(op.GetName()))
	}

	return s.report(op, p.abandoning)
}

// showIsolationFacts replaces the need for isolation that only the lost quorum
// asks for with the need the checks make once the isolation is given: those
// checks carry the facts the operator confirms it by. The dry-run that gets
// them changes nothing, and its confirmation is not kept.
func (s *session) showIsolationFacts(ctx context.Context, p plan, dec decisions, reqID string, unmet []need) []need {
	i := slices.IndexFunc(unmet, func(n need) bool {
		return n.kind == confIsolation && !slices.ContainsFunc(n.checks, func(c *dcctlv1.Check) bool {
			return c.GetEffect() == dcctlv1.Check_CONFIRM
		})
	})
	if i < 0 {
		return unmet
	}

	conf := &dcctlv1.Confirmation{Isolation: true}
	if c := dec.confirmation(); c != nil {
		conf.NotMaxLsn, conf.MaxDiscardBytes = c.GetNotMaxLsn(), c.GetMaxDiscardBytes()
	}

	op, err := call(ctx, s, func(ctx context.Context) (*dcctlv1.Operation, error) {
		return p.invoke(ctx, true, conf, reqID)
	})
	if err != nil {
		return unmet
	}

	needs, err := needsOf(op.GetChecks(), true)
	if err != nil {
		return unmet
	}

	if j := slices.IndexFunc(needs, func(n need) bool { return n.kind == confIsolation }); j >= 0 {
		unmet = slices.Clone(unmet)
		unmet[i] = needs[j]
	}

	return unmet
}
