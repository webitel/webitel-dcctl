package journal

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/webitel/webitel-go-kit/pkg/logging/redact"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/kv"
)

const (
	namePrefix = "journalEntries/"

	// maxDetail bounds detail and error.detail, in bytes.
	maxDetail = 4 << 10
	// maxEntrySize bounds the stored entry: every list, align and trim reads
	// up to max_entries of them per datacenter.
	maxEntrySize = 16 << 10
)

// ErrInvalidEntry means an entry cannot be journaled: it is incomplete, too
// big, or looks as if it holds a secret.
var ErrInvalidEntry = errors.New("invalid journal entry")

// keyRE is the part of a key after [kv.JournalPrefix]: 23 digits of UTC
// time and the id.
var keyRE = regexp.MustCompile(`^[0-9]{23}-[0-9A-Za-z]{1,32}$`)

// secretKeyRE matches a params or metadata key, in snake_case, whose last
// words name a secret: `password`, `api_key`, `session_token`. A name that
// only mentions one (`token_ttl`, `secret_name`) is not a secret.
var secretKeyRE = regexp.MustCompile(`(?:^|_)(?:password|passwd|secret|secret_?key|token|api_?key|authorization|credentials?|private_?key)$`)

// secretTextRE matches a secret stated in text: `token: abc`, `password=abc`
// or a bearer token. The last group is the value.
var secretTextRE = regexp.MustCompile(`(?i)\b(?:password|passwd|secret|token|api[-_ ]?key|authorization)[ \t]*[:=][ \t]*((?:bearer[ \t]+)?[^\s,;]+)|\bbearer[ \t]+([a-z0-9._~+/=-]{8,})`)

// snake returns key in lower snake_case: `apiKey` and `Api-Key` become
// `api_key`.
func snake(key string) string {
	var b strings.Builder

	var prev rune

	for _, r := range key {
		switch {
		case unicode.IsUpper(r):
			if unicode.IsLower(prev) || unicode.IsDigit(prev) {
				b.WriteByte('_')
			}

			b.WriteRune(unicode.ToLower(r))
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}

		prev = r
	}

	return b.String()
}

func isSecretKey(key string) bool {
	k := snake(key)

	return k != "page_token" && secretKeyRE.MatchString(k)
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidEntry, fmt.Sprintf(format, args...))
}

// validate checks the structure of e: what the journal cannot store.
func validate(e *dcctlv1.JournalEntry) error {
	if err := validateAction(e); err != nil {
		return err
	}

	switch {
	case e.GetResult() == dcctlv1.JournalEntry_RESULT_UNSPECIFIED:
		return invalid("result is not set")
	case len(e.GetDetail()) > maxDetail:
		return invalid("detail is longer than %d bytes", maxDetail)
	case len(e.GetError().GetDetail()) > maxDetail:
		return invalid("error.detail is longer than %d bytes", maxDetail)
	}

	return nil
}

func validateAction(e *dcctlv1.JournalEntry) error {
	switch e.GetType() {
	case dcctlv1.JournalEntry_OPERATION:
		if e.GetOperationType() == dcctlv1.Operation_TYPE_UNSPECIFIED {
			return invalid("type OPERATION needs operation_type")
		}
	case dcctlv1.JournalEntry_STEP:
		if e.GetStepType() == dcctlv1.Operation_Step_TYPE_UNSPECIFIED {
			return invalid("type STEP needs step_type")
		}
	case dcctlv1.JournalEntry_SYSTEM:
		if e.GetSystemAction() == dcctlv1.JournalEntry_SYSTEM_ACTION_UNSPECIFIED {
			return invalid("type SYSTEM needs system_action")
		}
	case dcctlv1.JournalEntry_ALARM:
		if e.GetAlarmType() == dcctlv1.Status_Alarm_TYPE_UNSPECIFIED {
			return invalid("type ALARM needs alarm_type")
		}
	case dcctlv1.JournalEntry_TYPE_UNSPECIFIED:
		return invalid("type is not set")
	default:
		return invalid("unknown type %d", e.GetType())
	}

	return nil
}

// redactEntry replaces the secrets in e, and reports whether it found any:
// the value of a params or error metadata key that names a secret, and the
// value after `token:`, `password=` or `Bearer` in text. The rest of the
// entry is kept, so an error that mentions a token is still journaled.
func redactEntry(e *dcctlv1.JournalEntry) bool {
	var found bool

	redactText := func(s *string) {
		if out, ok := redactText(*s); ok {
			*s, found = out, true
		}
	}

	redactText(&e.Detail)

	if er := e.GetError(); er != nil {
		redactText(&er.Detail)

		for k, v := range er.GetMetadata() {
			if isSecretKey(k) {
				er.Metadata[k], found = secretValue(v == ""), true

				continue
			}

			redactText(&v)
			er.Metadata[k] = v
		}
	}

	if e.GetParams() != nil && redactStruct(e.GetParams()) {
		found = true
	}

	return found
}

func redactStruct(s *structpb.Struct) bool {
	var found bool

	for k, v := range s.GetFields() {
		if isSecretKey(k) {
			s.Fields[k], found = structpb.NewStringValue(secretValue(isEmptyValue(v))), true

			continue
		}

		if redactValue(v) {
			found = true
		}
	}

	return found
}

func redactValue(v *structpb.Value) bool {
	switch k := v.GetKind().(type) {
	case *structpb.Value_StructValue:
		return redactStruct(k.StructValue)
	case *structpb.Value_ListValue:
		var found bool

		for _, item := range k.ListValue.GetValues() {
			if redactValue(item) {
				found = true
			}
		}

		return found
	case *structpb.Value_StringValue:
		if out, ok := redactText(k.StringValue); ok {
			v.Kind = &structpb.Value_StringValue{StringValue: out}

			return true
		}
	}

	return false
}

func isEmptyValue(v *structpb.Value) bool {
	switch k := v.GetKind().(type) {
	case nil, *structpb.Value_NullValue:
		return true
	case *structpb.Value_StringValue:
		return k.StringValue == ""
	case *structpb.Value_StructValue:
		return len(k.StructValue.GetFields()) == 0
	case *structpb.Value_ListValue:
		return len(k.ListValue.GetValues()) == 0
	}

	return false
}

// secretValue is what is recorded in place of a secret: `set` or `empty`,
// as in the log. A policy that drops secrets leaves the value empty.
func secretValue(empty bool) string {
	out, keep := redact.Render(redact.ClassSecret, "", empty)
	if !keep {
		return ""
	}

	return out
}

// redactText replaces the secret values stated in s.
func redactText(s string) (string, bool) {
	var found bool

	out := secretTextRE.ReplaceAllStringFunc(s, func(m string) string {
		found = true

		sub := secretTextRE.FindStringSubmatch(m)

		for _, v := range sub[1:] {
			if v != "" {
				return strings.TrimSuffix(m, v) + secretValue(false)
			}
		}

		return m
	})

	return out, found
}

// nameOf returns the resource name of a key.
func nameOf(key string) string {
	return namePrefix + strings.TrimPrefix(key, kv.JournalPrefix)
}

// suffixOf returns `<time>-<id>` of a key, and whether the key is a journal
// key.
func suffixOf(key string) (string, bool) {
	s, ok := strings.CutPrefix(key, kv.JournalPrefix)
	if !ok || !keyRE.MatchString(s) {
		return "", false
	}

	return s, true
}
