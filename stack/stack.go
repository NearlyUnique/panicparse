// Copyright 2015 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

// Package stack analyzes stack dump of Go processes and simplifies it.
//
// It is mostly useful on servers will large number of identical goroutines,
// making the crash dump harder to read than strictly necesary.
package stack

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const lockedToThread = "locked to thread"

var (
	reRoutineHeader = regexp.MustCompile("^goroutine (\\d+) \\[([^\\]]+)\\]\\:$")
	reMinutes       = regexp.MustCompile("^(\\d+) minutes$")
	reUnavail       = regexp.MustCompile("^(?:\t| +)goroutine running on other thread; stack unavailable")
	// - Sometimes the source file comes up as "<autogenerated>".
	// - Sometimes the tab is replaced with spaces.
	// - The +0x123 byte offset is not included with generated code, e.g. unnamed
	//   functions "func·006()" which is generally go func() { ... }() statements.
	// - C calls may have fp=0x123 sp=0x123 appended. These are discarded.
	reFile = regexp.MustCompile("^(?:\t| +)(\\<autogenerated\\>|.+\\.(?:c|go|s))\\:(\\d+)(?:| \\+0x[0-9a-f]+)(?:| fp=0x[0-9a-f]+ sp=0x[0-9a-f]+)$")
	// Sadly, it doesn't note the goroutine number so we could cascade them per
	// parenthood.
	reCreated = regexp.MustCompile("^created by (.+)$")
	reFunc    = regexp.MustCompile("^(.+)\\((.*)\\)$")
	reElided  = regexp.MustCompile("^\\.\\.\\.additional frames elided\\.\\.\\.$")
	// Include frequent GOROOT value on Windows, distro provided and user
	// installed path. This simplifies the user's life when processing a trace
	// generated on another VM.
	// TODO(maruel): Guess the path automatically via traces containing the
	// 'runtime' package, which is very frequent. This would be "less bad" than
	// throwing up random values at the parser.
	goroots = []string{runtime.GOROOT(), "c:/go", "/usr/lib/go", "/usr/local/go"}
)

type Function struct {
	Raw string
}

// String is the fully qualified function name.
//
// Sadly Go is a bit confused when the package name doesn't match the directory
// containing the source file and will use the directory name instead of the
// real package name.
func (f Function) String() string {
	s, _ := url.QueryUnescape(f.Raw)
	return s
}

// Name is the naked function name.
func (f Function) Name() string {
	parts := strings.SplitN(filepath.Base(f.Raw), ".", 2)
	if len(parts) == 1 {
		return parts[0]
	}
	return parts[1]
}

// PkgName is the package name for this function reference.
func (f Function) PkgName() string {
	parts := strings.SplitN(filepath.Base(f.Raw), ".", 2)
	if len(parts) == 1 {
		return ""
	}
	s, _ := url.QueryUnescape(parts[0])
	return s
}

// PkgDotName returns "<package>.<func>" format.
func (f Function) PkgDotName() string {
	parts := strings.SplitN(filepath.Base(f.Raw), ".", 2)
	s, _ := url.QueryUnescape(parts[0])
	if len(parts) == 1 {
		return parts[0]
	}
	if s != "" || parts[1] != "" {
		return s + "." + parts[1]
	}
	return ""
}

// IsExported returns true if the function is exported.
func (f Function) IsExported() bool {
	name := f.Name()
	parts := strings.Split(name, ".")
	r, _ := utf8.DecodeRuneInString(parts[len(parts)-1])
	if unicode.ToUpper(r) == r {
		return true
	}
	return f.PkgName() == "main" && name == "main"
}

// Arg is an argument on a Call.
type Arg struct {
	Value uint64 // Value is the raw value as found in the stack trace
	Name  string // Name is a pseudo name given to the argument
}

// IsPtr returns true if we guess it's a pointer. It's only a guess, it can be
// easily be confused by a bitmask.
func (a *Arg) IsPtr() bool {
	// Assumes all pointers are above 16Mb and positive.
	return a.Value > 16*1024*1024 && a.Value < math.MaxInt64
}

func (a Arg) String() string {
	if a.Name != "" {
		return a.Name
	}
	if a.Value == 0 {
		return "0"
	}
	return fmt.Sprintf("0x%x", a.Value)
}

// Args is a series of function call arguments.
type Args struct {
	Values    []Arg    // Values is the arguments as shown on the stack trace. They are mangled via simplification.
	Processed []string // Processed is the arguments generated from processing the source files. It can have a length lower than Values.
	Elided    bool     // If set, it means there was a trailing ", ..."
}

func (a Args) String() string {
	var v []string
	if len(a.Processed) != 0 {
		v = make([]string, 0, len(a.Processed))
		for _, item := range a.Processed {
			v = append(v, item)
		}
	} else {
		v = make([]string, 0, len(a.Values))
		for _, item := range a.Values {
			v = append(v, item.String())
		}
	}
	if a.Elided {
		v = append(v, "...")
	}
	return strings.Join(v, ", ")
}

func (a *Args) Equal(r *Args) bool {
	if a.Elided != r.Elided || len(a.Values) != len(r.Values) {
		return false
	}
	for i, l := range a.Values {
		if l != r.Values[i] {
			return false
		}
	}
	return true
}

// Similar returns true if the two Args are equal or almost but not quite
// equal.
func (a *Args) Similar(r *Args) bool {
	if a.Elided != r.Elided || len(a.Values) != len(r.Values) {
		return false
	}
	for i, l := range a.Values {
		if l.IsPtr() != r.Values[i].IsPtr() || (!l.IsPtr() && l != r.Values[i]) {
			return false
		}
	}
	return true
}

// Merge merges two similar Args, zapping out differences.
func (l *Args) Merge(r *Args) Args {
	out := Args{
		Values: make([]Arg, len(l.Values)),
		Elided: l.Elided,
	}
	for i := range l.Values {
		if l.Values[i] != r.Values[i] {
			out.Values[i].Name = "*"
			out.Values[i].Value = l.Values[i].Value
		} else {
			out.Values[i] = l.Values[i]
		}
	}
	return out
}

// Call is an item in the stack trace.
type Call struct {
	SourcePath string   // Full path name of the source file
	Line       int      // Line number
	Func       Function // Fully qualified function name (encoded).
	Args       Args     // Call arguments
}

func (c *Call) Equal(r *Call) bool {
	return c.SourcePath == r.SourcePath && c.Line == r.Line && c.Func == r.Func && c.Args.Equal(&r.Args)
}

// Similar returns true if the two Call are equal or almost but not quite
// equal.
func (c *Call) Similar(r *Call) bool {
	return c.SourcePath == r.SourcePath && c.Line == r.Line && c.Func == r.Func && c.Args.Similar(&r.Args)
}

// Merge merges two similar Call, zapping out differences.
func (l *Call) Merge(r *Call) Call {
	return Call{
		SourcePath: l.SourcePath,
		Line:       l.Line,
		Func:       l.Func,
		Args:       l.Args.Merge(&r.Args),
	}
}

// SourceName returns the base file name of the source file.
func (c *Call) SourceName() string {
	return filepath.Base(c.SourcePath)
}

// SourceLine returns "source.go:line", including only the base file name.
func (c *Call) SourceLine() string {
	return fmt.Sprintf("%s:%d", c.SourceName(), c.Line)
}

// FullSourceLine returns "/path/to/source.go:line".
func (c *Call) FullSourceLine() string {
	return fmt.Sprintf("%s:%d", c.SourcePath, c.Line)
}

// PkgSource is one directory plus the file name of the source file.
func (c *Call) PkgSource() string {
	return filepath.Join(filepath.Base(filepath.Dir(c.SourcePath)), c.SourceName())
}

const testMainSource = "_test" + string(os.PathSeparator) + "_testmain.go"

// IsStdlib returns true if it is a Go standard library function. This includes
// the 'go test' generated main executable.
func (c *Call) IsStdlib() bool {
	for _, goroot := range goroots {
		if strings.HasPrefix(c.SourcePath, goroot) {
			return true
		}
	}
	// Consider _test/_testmain.go as stdlib since it's injected by "go test".
	return c.PkgSource() == testMainSource
}

// IsMain returns true if it is in the main package.
func (c *Call) IsPkgMain() bool {
	return c.Func.PkgName() == "main"
}

// Goroutine represents the signature of one or multiple goroutines.
type Signature struct {
	// Use git grep 'gopark(|unlock)\(' to find them all plus everything listed
	// in runtime/traceback.go. Valid values includes:
	//     - chan send, chan receive, select
	//     - finalizer wait, mark wait (idle),
	//     - Concurrent GC wait, GC sweep wait, force gc (idle)
	//     - IO wait, panicwait
	//     - semacquire, semarelease
	//     - sleep, timer goroutine (idle)
	//     - trace reader (blocked)
	// Stuck cases:
	//     - chan send (nil chan), chan receive (nil chan), select (no cases)
	// Runnable states:
	//    - idle, runnable, running, syscall, waiting, dead, enqueue, copystack,
	// Scan states:
	//    - scan, scanrunnable, scanrunning, scansyscall, scanwaiting, scandead,
	//      scanenqueue
	State       string
	Sleep       int    // Wait time in minutes, if applicable.
	Locked      bool   // Locked to an OS thread.
	Stack       []Call // Call stack.
	StackElided bool   // Happens when there's >100 items in Stack, currently hardcoded in package runtime.
	CreatedBy   Call   // Which other goroutine which created this one.
}

func (l *Signature) Equal(r *Signature) bool {
	// Ignore Sleep and Locked.
	if l.State != r.State || len(l.Stack) != len(r.Stack) || !l.CreatedBy.Equal(&r.CreatedBy) || r.StackElided != l.StackElided {
		return false
	}
	for i := range l.Stack {
		if !l.Stack[i].Equal(&r.Stack[i]) {
			return false
		}
	}
	return true
}

// Similar returns true if the two Signature are equal or almost but not quite
// equal.
func (l *Signature) Similar(r *Signature) bool {
	// Ignore Sleep and Locked.
	if l.State != r.State || len(l.Stack) != len(r.Stack) || !l.CreatedBy.Similar(&r.CreatedBy) || r.StackElided != l.StackElided {
		return false
	}
	for i := range l.Stack {
		if !l.Stack[i].Similar(&r.Stack[i]) {
			return false
		}
	}
	return true
}

// Merge merges two similar Signature, zapping out differences.
func (l *Signature) Merge(r *Signature) *Signature {
	out := &Signature{
		State:     l.State,
		Sleep:     (l.Sleep + r.Sleep + 1) / 2,
		Locked:    l.Locked || r.Locked,
		Stack:     make([]Call, len(l.Stack)),
		CreatedBy: l.CreatedBy,
	}
	for i := range l.Stack {
		out.Stack[i] = l.Stack[i].Merge(&r.Stack[i])
	}
	return out
}

// Less compares two signautre, where the ones that are less are more
// important, so they come up front. A Signature with more private functions is
// 'less' so it is at the top. Inversely, a Signature with only public
// functions is 'more' so it is at the bottom.
func (l *Signature) Less(r *Signature) bool {
	// Ignore Sleep and Locked.
	lStdlib := 0
	lPrivate := 0
	for _, s := range l.Stack {
		if s.IsStdlib() {
			lStdlib++
		} else {
			lPrivate++
		}
	}
	rStdlib := 0
	rPrivate := 0
	for _, s := range r.Stack {
		if s.IsStdlib() {
			rStdlib++
		} else {
			rPrivate++
		}
	}
	if lPrivate > rPrivate {
		return true
	}
	if lPrivate < rPrivate {
		return false
	}
	if lStdlib > rStdlib {
		return false
	}
	if lStdlib < rStdlib {
		return true
	}

	// Stack lengths are the same.
	for x := range l.Stack {
		if l.Stack[x].Func.Raw < r.Stack[x].Func.Raw {
			return true
		}
		if l.Stack[x].Func.Raw > r.Stack[x].Func.Raw {
			return true
		}
		if l.Stack[x].PkgSource() < r.Stack[x].PkgSource() {
			return true
		}
		if l.Stack[x].PkgSource() > r.Stack[x].PkgSource() {
			return true
		}
		if l.Stack[x].Line < r.Stack[x].Line {
			return true
		}
		if l.Stack[x].Line > r.Stack[x].Line {
			return true
		}
	}
	if l.State < r.State {
		return true
	}
	if l.State > r.State {
		return false
	}
	return false
}

// Goroutine represents the state of one goroutine.
type Goroutine struct {
	Signature
	ID    int
	First bool // First is the goroutine first printed, normally the one that crashed.
}

// Bucketize returns the number of similar goroutines.
//
// It will aggressively deduplicate similar looking stack traces differing only
// with pointer values if aggressive is true.
func Bucketize(goroutines []Goroutine, aggressive bool) map[*Signature][]Goroutine {
	out := map[*Signature][]Goroutine{}
	// O(n²). Fix eventually.
	for _, routine := range goroutines {
		found := false
		for key := range out {
			// When a match is found, this effectively drops the other goroutine ID.
			if !aggressive {
				if key.Equal(&routine.Signature) {
					found = true
					out[key] = append(out[key], routine)
					break
				}
			} else {
				if key.Similar(&routine.Signature) {
					found = true
					if !key.Equal(&routine.Signature) {
						// Almost but not quite equal. There's different pointers passed
						// around but the same values. Zap out the different values.
						newKey := key.Merge(&routine.Signature)
						out[newKey] = append(out[key], routine)
						delete(out, key)
					} else {
						out[key] = append(out[key], routine)
					}
					break
				}
			}
		}
		if !found {
			key := &Signature{}
			*key = routine.Signature
			out[key] = []Goroutine{routine}
		}
	}
	return out
}

// Bucket is a stack trace signature.
type Bucket struct {
	Signature
	Routines []Goroutine
}

func (b *Bucket) First() bool {
	for _, r := range b.Routines {
		if r.First {
			return true
		}
	}
	return false
}

// Less does reverse sort.
func (b *Bucket) Less(r *Bucket) bool {
	if b.First() {
		return true
	}
	if r.First() {
		return false
	}
	return b.Signature.Less(&r.Signature)
}

// Buckets is a list of Bucket sorted by repeation count.
type Buckets []Bucket

func (b Buckets) Len() int {
	return len(b)
}

func (b Buckets) Less(i, j int) bool {
	return b[i].Less(&b[j])
}

func (b Buckets) Swap(i, j int) {
	b[j], b[i] = b[i], b[j]
}

// SortBuckets creates a list of Bucket from each goroutine stack trace count.
func SortBuckets(buckets map[*Signature][]Goroutine) Buckets {
	out := make(Buckets, 0, len(buckets))
	for signature, count := range buckets {
		out = append(out, Bucket{*signature, count})
	}
	sort.Sort(out)
	return out
}

// ParseDump processes the output from runtime.Stack().
//
// It supports piping from another command and assumes there is junk before the
// actual stack trace. The junk is streamed to out.
func ParseDump(r io.Reader, out io.Writer) ([]Goroutine, error) {
	goroutines := make([]Goroutine, 0, 16)
	var goroutine *Goroutine
	scanner := bufio.NewScanner(r)
	scanner.Split(bufio.ScanLines)
	// TODO(maruel): Use a formal state machine. Patterns follows:
	// - reRoutineHeader
	//   Either:
	//     - reUnavail
	//     - reFunc + reFile in a loop
	//     - reElided
	//   Optionally ends with:
	//     - reCreated + reFile
	// Between each goroutine stack dump: an empty line
	created := false
	firstLine := false
	for scanner.Scan() {
		line := scanner.Text()
		if len(line) == 0 {
			if goroutine == nil {
				_, _ = io.WriteString(out, line+"\n")
			}
			goroutine = nil
			continue
		}

		if goroutine == nil {
			if match := reRoutineHeader.FindStringSubmatch(line); match != nil {
				if id, err := strconv.Atoi(match[1]); err == nil {
					// See runtime/traceback.go.
					// "<state>, \d+ minutes, locked to thread"
					items := strings.Split(match[2], ", ")
					sleep := 0
					locked := false
					for i := 1; i < len(items); i++ {
						if items[i] == lockedToThread {
							locked = true
							continue
						}
						// Look for duration, if any.
						if match2 := reMinutes.FindStringSubmatch(items[i]); match2 != nil {
							sleep, _ = strconv.Atoi(match2[1])
						}
					}
					goroutines = append(goroutines, Goroutine{
						Signature: Signature{State: items[0], Sleep: sleep, Locked: locked, Stack: []Call{}},
						ID:        id,
						First:     len(goroutines) == 0,
					})
					goroutine = &goroutines[len(goroutines)-1]
					firstLine = true
					continue
				}
			}
			_, _ = io.WriteString(out, line+"\n")
			continue
		}

		if firstLine {
			firstLine = false
			if match := reUnavail.FindStringSubmatch(line); match != nil {
				// Generate a fake stack entry.
				goroutine.Stack = []Call{{SourcePath: "<unavailable>"}}
				continue
			}
		}

		if match := reFile.FindStringSubmatch(line); match != nil {
			// Triggers after a reFunc or a reCreated.
			num, err := strconv.Atoi(match[2])
			if err != nil {
				return goroutines, fmt.Errorf("failed to parse int on line: \"%s\"", line)
			}
			if created {
				created = false
				goroutine.CreatedBy.SourcePath = match[1]
				goroutine.CreatedBy.Line = num
			} else {
				i := len(goroutine.Stack) - 1
				goroutine.Stack[i].SourcePath = match[1]
				goroutine.Stack[i].Line = num
			}
		} else if match := reCreated.FindStringSubmatch(line); match != nil {
			created = true
			goroutine.CreatedBy.Func.Raw = match[1]
		} else if match := reFunc.FindStringSubmatch(line); match != nil {
			args := Args{}
			for _, a := range strings.Split(match[2], ", ") {
				if a == "..." {
					args.Elided = true
					continue
				}
				if a == "" {
					// Remaining values were dropped.
					break
				}
				v, err := strconv.ParseUint(a, 0, 64)
				if err != nil {
					// TODO(maruel): If this ever happens, it should be handled more
					// gracefully.
					return nil, err
				}
				args.Values = append(args.Values, Arg{Value: v})
			}
			goroutine.Stack = append(goroutine.Stack, Call{Func: Function{match[1]}, Args: args})
		} else if match := reElided.FindStringSubmatch(line); match != nil {
			goroutine.StackElided = true
		} else {
			_, _ = io.WriteString(out, line+"\n")
			goroutine = nil
		}
	}
	nameArguments(goroutines)
	return goroutines, scanner.Err()
}

// Private stuff.

func nameArguments(goroutines []Goroutine) {
	// Set a name for any pointer occuring more than once.
	type object struct {
		args      []*Arg
		inPrimary bool
		id        int
	}
	objects := map[uint64]object{}
	// Enumerate all the arguments.
	for i := range goroutines {
		for j := range goroutines[i].Stack {
			for k := range goroutines[i].Stack[j].Args.Values {
				arg := goroutines[i].Stack[j].Args.Values[k]
				if arg.IsPtr() {
					objects[arg.Value] = object{
						args:      append(objects[arg.Value].args, &goroutines[i].Stack[j].Args.Values[k]),
						inPrimary: objects[arg.Value].inPrimary || i == 0,
					}
				}
			}
		}
		// CreatedBy.Args is never set.
	}
	order := uint64Slice{}
	for k, obj := range objects {
		if len(obj.args) > 1 && obj.inPrimary {
			order = append(order, k)
		}
	}
	sort.Sort(order)
	nextID := 1
	for _, k := range order {
		for _, arg := range objects[k].args {
			arg.Name = fmt.Sprintf("#%d", nextID)
		}
		nextID++
	}

	// Now do the rest. This is done so the output is deterministic.
	order = uint64Slice{}
	for k := range objects {
		order = append(order, k)
	}
	sort.Sort(order)
	for _, k := range order {
		// Process the remaining pointers, they were not referenced by primary
		// thread so will have higher IDs.
		if objects[k].inPrimary {
			continue
		}
		for _, arg := range objects[k].args {
			arg.Name = fmt.Sprintf("#%d", nextID)
		}
		nextID++
	}
}

type uint64Slice []uint64

func (a uint64Slice) Len() int           { return len(a) }
func (a uint64Slice) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a uint64Slice) Less(i, j int) bool { return a[i] < a[j] }
