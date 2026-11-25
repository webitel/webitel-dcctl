package journal

import (
	"errors"
	"fmt"
	"sync"

	"go.einride.tech/aip/filtering"
	expr "google.golang.org/genproto/googleapis/api/expr/v1alpha1"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
)

// matcher tells whether an entry passes a filter.
type matcher func(*dcctlv1.JournalEntry) bool

// fields are the entry fields a filter may name.
var fields = map[string]func(*dcctlv1.JournalEntry) string{
	"type":       func(e *dcctlv1.JournalEntry) string { return e.GetType().String() },
	"datacenter": func(e *dcctlv1.JournalEntry) string { return e.GetDatacenter() },
	"operation":  func(e *dcctlv1.JournalEntry) string { return e.GetOperation() },
}

// declarations declares what an AIP-160 filter of the journal may use: the
// string fields in [fields] and, on strings, = != AND OR NOT and the
// implicit AND of a sequence. Everything else fails the type check.
var declarations = sync.OnceValues(func() (*filtering.Declarations, error) {
	opts := make([]filtering.DeclarationOption, 0, 6+len(fields))
	opts = append(opts,
		filtering.DeclareFunction(filtering.FunctionEquals,
			filtering.NewFunctionOverload(filtering.FunctionEquals+"_string", filtering.TypeBool, filtering.TypeString, filtering.TypeString)),
		filtering.DeclareFunction(filtering.FunctionNotEquals,
			filtering.NewFunctionOverload(filtering.FunctionNotEquals+"_string", filtering.TypeBool, filtering.TypeString, filtering.TypeString)),
		filtering.DeclareFunction(filtering.FunctionAnd, filtering.NewFunctionOverload(filtering.FunctionOverloadAndBool, filtering.TypeBool, filtering.TypeBool, filtering.TypeBool)),
		filtering.DeclareFunction(filtering.FunctionFuzzyAnd, filtering.NewFunctionOverload(filtering.FunctionFuzzyAnd+"_bool", filtering.TypeBool, filtering.TypeBool, filtering.TypeBool)),
		filtering.DeclareFunction(filtering.FunctionOr, filtering.NewFunctionOverload(filtering.FunctionOverloadOrBool, filtering.TypeBool, filtering.TypeBool, filtering.TypeBool)),
		filtering.DeclareFunction(filtering.FunctionNot, filtering.NewFunctionOverload(filtering.FunctionOverloadNotBool, filtering.TypeBool, filtering.TypeBool)),
	)

	for name := range fields {
		opts = append(opts, filtering.DeclareIdent(name, filtering.TypeString))
	}

	return filtering.NewDeclarations(opts...)
})

// compileFilter parses an AIP-160 filter and returns the matcher of it. An
// empty filter matches every entry. The error is for the client: it says
// what is wrong with the filter.
func compileFilter(filter string) (matcher, error) {
	if filter == "" {
		return func(*dcctlv1.JournalEntry) bool { return true }, nil
	}

	decls, err := declarations()
	if err != nil {
		return nil, fmt.Errorf("declare journal filter: %w", err)
	}

	parsed, err := filtering.ParseFilterString(filter, decls)
	if err != nil {
		return nil, &filterError{err}
	}

	m, err := build(parsed.CheckedExpr.GetExpr())
	if err != nil {
		return nil, &filterError{err}
	}

	return m, nil
}

// filterError is a filter the client wrote wrong, as opposed to a failure to
// set filtering up.
type filterError struct{ error }

func (e *filterError) Unwrap() error { return e.error }

func build(e *expr.Expr) (matcher, error) {
	call := e.GetCallExpr()
	if call == nil {
		return nil, errors.New("the filter must be a comparison of a field with a value")
	}

	args := call.GetArgs()

	switch call.GetFunction() {
	case filtering.FunctionAnd, filtering.FunctionFuzzyAnd:
		return combine(args, func(l, r bool) bool { return l && r })
	case filtering.FunctionOr:
		return combine(args, func(l, r bool) bool { return l || r })
	case filtering.FunctionNot:
		m, err := build(args[0])
		if err != nil {
			return nil, err
		}

		return func(e *dcctlv1.JournalEntry) bool { return !m(e) }, nil
	case filtering.FunctionEquals:
		return compare(args, true)
	case filtering.FunctionNotEquals:
		return compare(args, false)
	}

	return nil, fmt.Errorf("function %q is not supported", call.GetFunction())
}

func combine(args []*expr.Expr, op func(l, r bool) bool) (matcher, error) {
	left, err := build(args[0])
	if err != nil {
		return nil, err
	}

	right, err := build(args[1])
	if err != nil {
		return nil, err
	}

	return func(e *dcctlv1.JournalEntry) bool { return op(left(e), right(e)) }, nil
}

// compare returns the matcher of `field = "value"` (equal) or `field !=
// "value"`.
func compare(args []*expr.Expr, equal bool) (matcher, error) {
	name := args[0].GetIdentExpr().GetName()

	get, ok := fields[name]
	if !ok {
		return nil, errors.New("the left side must be one of type, datacenter, operation")
	}

	c := args[1].GetConstExpr()
	if c == nil {
		return nil, fmt.Errorf("the value of %s must be a quoted string", name)
	}

	want := c.GetStringValue()

	if name == "type" {
		if err := checkType(want); err != nil {
			return nil, err
		}
	}

	return func(e *dcctlv1.JournalEntry) bool { return (get(e) == want) == equal }, nil
}

func checkType(v string) error {
	n, ok := dcctlv1.JournalEntry_Type_value[v]
	if !ok || n == int32(dcctlv1.JournalEntry_TYPE_UNSPECIFIED) {
		return fmt.Errorf("type %q is unknown, use OPERATION, STEP, SYSTEM or ALARM", v)
	}

	return nil
}
