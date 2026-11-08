package cli

import (
	"flag"
	"fmt"
	"io"
	"strings"
)

// multi is a repeatable string flag that also splits on commas when asked.
type multi struct {
	vals  *[]string
	split bool
}

func (m multi) String() string {
	if m.vals == nil {
		return ""
	}
	return strings.Join(*m.vals, ",")
}

func (m multi) Set(v string) error {
	if m.split {
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				*m.vals = append(*m.vals, p)
			}
		}
		return nil
	}
	*m.vals = append(*m.vals, v)
	return nil
}

type flags struct {
	fs *flag.FlagSet
}

func newFlags(name string) *flags {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return &flags{fs: fs}
}

func (f *flags) str(p *string, def, usage string, names ...string) {
	for _, n := range names {
		f.fs.StringVar(p, n, def, usage)
	}
}

func (f *flags) boolean(p *bool, usage string, names ...string) {
	for _, n := range names {
		f.fs.BoolVar(p, n, false, usage)
	}
}

func (f *flags) integer(p *int, def int, usage string, names ...string) {
	for _, n := range names {
		f.fs.IntVar(p, n, def, usage)
	}
}

func (f *flags) list(p *[]string, split bool, usage string, names ...string) {
	for _, n := range names {
		f.fs.Var(multi{vals: p, split: split}, n, usage)
	}
}

// parse accepts flags anywhere among positionals ("kb add learning Title -d x").
func (f *flags) parse(args []string) ([]string, error) {
	var pos []string
	for len(args) > 0 {
		a := args[0]
		if a == "--" {
			pos = append(pos, args[1:]...)
			break
		}
		if strings.HasPrefix(a, "-") && a != "-" && len(a) > 1 {
			name := strings.TrimLeft(a, "-")
			hasValue := false
			if i := strings.Index(name, "="); i >= 0 {
				name, hasValue = name[:i], true
			}
			if name == "h" || name == "help" {
				return nil, flag.ErrHelp
			}
			fl := f.fs.Lookup(name)
			if fl == nil {
				return nil, fmt.Errorf("unknown flag %s", a)
			}
			n := 1
			if bf, ok := fl.Value.(interface{ IsBoolFlag() bool }); !(ok && bf.IsBoolFlag()) && !hasValue {
				n = 2
			}
			if n > len(args) {
				return nil, fmt.Errorf("flag %s needs a value", a)
			}
			if err := f.fs.Parse(args[:n]); err != nil {
				return nil, err
			}
			args = args[n:]
			continue
		}
		pos = append(pos, a)
		args = args[1:]
	}
	return pos, nil
}
