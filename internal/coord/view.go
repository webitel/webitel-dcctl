package coord

import (
	"cmp"
	"slices"

	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/source"
)

// Status is what reading dcctl/state from a voter gave.
type Status string

const (
	StatusPresent Status = "present"
	// StatusAbsent is a voter that answered without the key.
	StatusAbsent Status = "absent"
	// StatusUninitialized is an arbiter that does not vote yet.
	StatusUninitialized Status = "uninitialized"
	StatusUnreachable   Status = "unreachable"
	// StatusInvalid is a record that cannot be decoded. It may be written
	// by a newer dcctl, so it is neither counted nor overwritten.
	StatusInvalid Status = "invalid"
)

// VoterState is the result of reading dcctl/state from one voter.
type VoterState struct {
	Name   string
	Kind   Kind
	Status Status
	// Reason is why the voter is unreachable or its record invalid.
	Reason source.Reason
	Err    error
	// State, Raw and Version are set for StatusPresent. Raw is the value as
	// stored, with the fields this version of dcctl does not know.
	State   kv.State
	Raw     []byte
	Version Version
	// Stale is set by [Evaluate] for a voter to align: it holds a lower
	// generation than the effective state, or it is a KV without the key,
	// or it holds another record of the effective generation, which cannot
	// be committed as the effective record is on a majority.
	Stale bool
	// Behind is set by [Evaluate] for a voter that holds the effective
	// record at a lower revision, as an update within the generation has
	// not reached it yet, or with another payload of its revision while a
	// majority holds the effective payload, as a writer that lost a race
	// left it. It is aligned too, but it is not stale.
	Behind bool
}

// Record is a generation and active datacenter with the voters that hold
// it. Voters holding one record may differ in the fields updated within a
// generation (revision, patroni_timeline, needs_recovery, forced); State
// and Raw are those of one of them, see [Evaluate].
type Record struct {
	State  kv.State
	Raw    []byte
	Voters []string
	// Settled is set when State is the payload of every voter of the
	// record at its revision, or of a majority of all voters. Two payloads
	// of one revision, from writers that raced, leave the record unsettled
	// until a majority holds one of them or a higher revision is written.
	// So do records written before revision existed, as all of them have
	// revision 0.
	Settled bool
}

// View is the state of the voters as one instance sees it.
type View struct {
	// Voters is in the order of the configured voters.
	Voters []VoterState
	// Majority is the number of voters a record needs to be valid, out of
	// all configured voters, reachable or not.
	Majority int
	// Voting counts the voters that answered and can take a write.
	Voting int
	// Quorum is set when the voters that answered and can take a write are
	// a majority. A KV without the key counts, although it accepts a CAS
	// from the current generation only once it is aligned; an
	// uninitialized arbiter does not. Quorum says nothing about which
	// records are committed.
	Quorum bool
	// Initialized is set when some voter holds dcctl/state. When it is not
	// and some voter is unreachable, the state may still exist there.
	Initialized bool
	// Effective is the valid record of the highest generation: on a
	// majority of voters or forced. It is nil when there is none or the
	// highest valid generation is in Conflict. A forced Effective may be
	// below an Undetermined record.
	Effective *Record
	// Uncommitted are the records above the effective generation, or any
	// record when nothing is valid, that are not forced and cannot be on a
	// majority even if every unreachable or invalid voter held them.
	// Records of the effective generation are among them when the effective
	// record is forced and not on a majority; when it is on a majority,
	// their voters are stale. Highest first.
	Uncommitted []Record
	// Undetermined are the records Uncommitted would hold but for the
	// unreachable or invalid voters, which may hold them and make them
	// committed. Highest first.
	Undetermined []Record
	// Conflict are two or more valid records of the highest valid
	// generation with different active datacenters.
	Conflict []Record
}

type recordKey struct {
	generation int64
	datacenter string
}

// group is one record and the indexes of the voters holding it.
type group struct {
	key     recordKey
	members []int
	forced  bool
}

// Evaluate builds the view of voters, which must be every configured voter.
// Two voters hold the same record when they have the same generation and
// active datacenter: the other fields change within a generation one voter
// at a time, and comparing them would take the majority from a committed
// generation while such an update is under way.
func Evaluate(voters []VoterState) View {
	v := View{
		Voters:   slices.Clone(voters),
		Majority: len(voters)/2 + 1,
	}

	var (
		groups []*group
		byKey  = make(map[recordKey]*group)
		voting int
		// unknown counts the voters that may hold any record.
		unknown int
	)

	for i, vs := range v.Voters {
		switch vs.Status {
		case StatusPresent:
			voting++
			v.Initialized = true
		case StatusAbsent:
			// An arbiter without state does not vote, see [StatusUninitialized].
			if vs.Kind == KindKV {
				voting++
			}

			continue
		case StatusInvalid:
			v.Initialized = true
			unknown++

			continue
		case StatusUnreachable:
			unknown++

			continue
		case StatusUninitialized:
			continue
		}

		k := recordKey{generation: vs.State.Generation, datacenter: vs.State.ActiveDatacenter}

		g, ok := byKey[k]
		if !ok {
			g = &group{key: k}
			byKey[k] = g
			groups = append(groups, g)
		}

		g.members = append(g.members, i)
		g.forced = g.forced || vs.State.Forced
	}

	v.Voting = voting
	v.Quorum = voting >= v.Majority

	valid := func(g *group) bool { return len(g.members) >= v.Majority || g.forced }

	var (
		haveTop bool
		topGen  int64
	)

	for _, g := range groups {
		if valid(g) && (!haveTop || g.key.generation > topGen) {
			topGen, haveTop = g.key.generation, true
		}
	}

	var top []*group

	for _, g := range groups {
		if valid(g) && g.key.generation == topGen {
			top = append(top, g)
		}
	}

	if len(top) == 1 {
		r := v.record(top[0])
		v.Effective = &r
	} else {
		slices.SortFunc(top, func(a, b *group) int { return cmp.Compare(a.key.datacenter, b.key.datacenter) })

		for _, g := range top {
			v.Conflict = append(v.Conflict, v.record(g))
		}
	}

	var above []*group

	// Other records of the generation of a forced effective record that is
	// not on a majority may be committed too.
	forcedMinority := v.Effective != nil && len(v.Effective.Voters) < v.Majority

	for _, g := range groups {
		sideOfForced := forcedMinority && g.key.generation == topGen
		if !valid(g) && (!haveTop || g.key.generation > topGen || sideOfForced) {
			above = append(above, g)
		}
	}

	slices.SortFunc(above, func(a, b *group) int {
		return cmp.Or(cmp.Compare(b.key.generation, a.key.generation), cmp.Compare(a.key.datacenter, b.key.datacenter))
	})

	for _, g := range above {
		if len(g.members)+unknown >= v.Majority {
			v.Undetermined = append(v.Undetermined, v.record(g))
		} else {
			v.Uncommitted = append(v.Uncommitted, v.record(g))
		}
	}

	if v.Effective != nil {
		o := v.overwrite()

		for i := range v.Voters {
			vs := &v.Voters[i]
			s, present := vs.State, vs.Status == StatusPresent
			vs.Stale = present && (s.Generation < topGen || o.lostRecord(s)) ||
				vs.Status == StatusAbsent && vs.Kind == KindKV
			vs.Behind = present && (o.lowerRevision(s) || o.lostRace(s))
		}
	}

	return v
}

// record returns g with the payload of one of its voters, so that every
// instance seeing the same voters picks the same one: of the highest
// revision; among those the payload a majority of all voters holds; without
// one, a forced voter first, then the payload most of the voters hold, then
// the first voter. See [Record.Settled].
func (v *View) record(g *group) Record {
	r := Record{Voters: make([]string, 0, len(g.members))}

	top := v.Voters[g.members[0]].State.Revision
	for _, i := range g.members[1:] {
		top = max(top, v.Voters[i].State.Revision)
	}

	best, bestForced, bestCount, atTop := -1, false, 0, 0

	for _, i := range g.members {
		s := v.Voters[i].State
		r.Voters = append(r.Voters, v.Voters[i].Name)

		if s.Revision != top {
			continue
		}

		atTop++
		count := 0

		for _, j := range g.members {
			if samePayload(s, v.Voters[j].State) {
				count++
			}
		}

		if best < 0 || v.better(s.Forced, count, bestForced, bestCount) {
			best, bestForced, bestCount = i, s.Forced, count
		}
	}

	r.State = v.Voters[best].State
	r.Raw = v.Voters[best].Raw
	r.Settled = bestCount >= v.Majority || bestCount == atTop

	return r
}

// better reports whether a payload of one revision, forced or not and held
// by count voters, goes before the best one so far.
func (v *View) better(forced bool, count int, bestForced bool, bestCount int) bool {
	if onMajority, bestOnMajority := count >= v.Majority, bestCount >= v.Majority; onMajority != bestOnMajority {
		return onMajority
	}

	if forced != bestForced {
		return forced
	}

	return count > bestCount
}

// holding returns the number of voters whose payload is s.
func (v *View) holding(s kv.State) int {
	n := 0

	for _, vs := range v.Voters {
		if vs.Status == StatusPresent && samePayload(vs.State, s) {
			n++
		}
	}

	return n
}

// overwrite tells which records the effective record of a view may be
// written over. What is on a majority is judged on the view; the rules then
// apply both to the voters of the view and to what a voter holds when it is
// read again right before the write, so that a record or payload committed
// in between is never written over on the strength of an older view.
type overwrite struct {
	effective kv.State
	// recordOnMajority is set when the voters of the effective record are a
	// majority: no other record of its generation can be committed.
	recordOnMajority bool
	// payloadOnMajority is set when the effective payload is on a majority:
	// no other payload of its revision can be committed.
	payloadOnMajority bool
}

// overwrite returns the rules of v, which has an effective record.
func (v *View) overwrite() overwrite {
	return overwrite{
		effective:         v.Effective.State,
		recordOnMajority:  len(v.Effective.Voters) >= v.Majority,
		payloadOnMajority: v.holding(v.Effective.State) >= v.Majority,
	}
}

// allows reports whether the effective record may be written over s.
func (o overwrite) allows(s kv.State) bool {
	return s.Generation < o.effective.Generation || o.lowerRevision(s) || o.lostRace(s) || o.lostRecord(s)
}

// lowerRevision reports whether s holds the effective record at a lower
// revision.
func (o overwrite) lowerRevision(s kv.State) bool {
	return sameRecord(s, o.effective) && s.Revision < o.effective.Revision
}

// lostRace reports whether s holds another payload of the effective revision
// while the effective payload is on a majority.
func (o overwrite) lostRace(s kv.State) bool {
	return o.payloadOnMajority && sameRecord(s, o.effective) && s.Revision == o.effective.Revision &&
		!samePayload(s, o.effective)
}

// lostRecord reports whether s is another record of the effective generation,
// not forced, while the effective record is on a majority.
func (o overwrite) lostRecord(s kv.State) bool {
	return o.recordOnMajority && s.Generation == o.effective.Generation && !sameRecord(s, o.effective) && !s.Forced
}

// sameRecord reports whether a and b are the same record: the same
// generation and active datacenter.
func sameRecord(a, b kv.State) bool {
	return a.Generation == b.Generation && a.ActiveDatacenter == b.ActiveDatacenter
}

// samePayload compares the fields that decisions are based on; UpdatedAt
// is for display only.
func samePayload(a, b kv.State) bool {
	return a.Generation == b.Generation &&
		a.Revision == b.Revision &&
		a.ActiveDatacenter == b.ActiveDatacenter &&
		a.OpID == b.OpID &&
		a.Forced == b.Forced &&
		a.PatroniTimeline == b.PatroniTimeline &&
		slices.Equal(a.NeedsRecovery, b.NeedsRecovery)
}
