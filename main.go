package main

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pborman/getopt/v2"
)

func agoR(d time.Duration, maxPrec int) (s string) {
	if maxPrec <= 0 {
		return ""
	}
	ranges := []struct {
		lt   time.Duration
		div  time.Duration
		unit string
	}{
		{time.Minute, time.Second, "s"},
		{time.Hour, time.Minute, "m"},
		{2 * day, time.Hour, "h"},
		{month, day, "d"},
		{3 * month, week, "w"},
		{2 * year, month, "y"},
	}
	div, unit := year, "y"
	for _, r := range ranges {
		if d < r.lt {
			div, unit = r.div, r.unit
			break
		}
	}
	v := int64(d.Seconds() / div.Seconds())
	r := time.Duration(d.Seconds()-float64(v)*div.Seconds()) * time.Second
	tail := ""
	if r > 1*time.Second {
		tail = agoR(r, maxPrec-1)
	}
	return fmt.Sprintf("%2d%s%s", v, unit, tail)
}

func ago(d time.Duration, maxPrec int) string {
	s := agoR(d, maxPrec)
	if d > 0*time.Second {
		return s + " ago"
	}
	return "in " + s
}

const defaultDirMode = 0755
const defaultBtrfsBin = "btrfs"

type snap struct {
	path    string
	created time.Time
}

func (s *snap) String() string {
	return s.path
}

func findSnaps(dir string) ([]*snap, error) {
	fis, err := ioutil.ReadDir(dir)
	if err != nil && os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		// If the directory does not exist, there are no snapshots.
		return nil, err
	}
	snaps := make([]*snap, 0, len(fis))
	for _, fi := range fis {
		snapPath := path.Join(dir, fi.Name())
		createdUnix, err := strconv.ParseInt(fi.Name(), 10, 64)
		if err != nil {
			return nil, err
		}
		created := time.Unix(createdUnix, 0)
		snaps = append(snaps, &snap{snapPath, created})
	}
	return snaps, nil
}

type bucket struct {
	interval time.Duration
	snaps    []*snap
}

func newBucket(interval time.Duration, size int) *bucket {
	if size <= 0 {
		panic("bug: bucket size must be positive")
	}
	return &bucket{
		interval: interval,
		snaps:    make([]*snap, 0, size),
	}
}

func (b bucket) String() string {
	return fmt.Sprintf("%s: %v", b.interval, b.snaps)
}

// cascade is a cascade of buckets which support hierarchical eviction.
type cascade []*bucket

func newCascade() cascade {
	return make(cascade, 0)
}

func (c *cascade) addBucket(b *bucketJSON) {
	*c = append(*c, &bucket{
		interval: time.Duration(*b.Interval),
		snaps:    make([]*snap, 0, *b.Size),
	})
}

// insert puts in snapshots into the top bucket. If that bucket is full, oldest
// snapshots are evicted to lower buckets. Any snapshots which don't fit the
// last bucket are returned in out.
//
// Insertion respect bucket intervals: TODO.
func (c cascade) insert(in []*snap) (out []*snap) {
	sort.Slice(in, func(i, j int) bool {
		return in[i].created.Before(in[j].created)
	})
	var overflow []*snap
	for _, b := range c {
		var prevCreated time.Time
		var insertAt int
		for i, s := range in {
			d := s.created.Sub(prevCreated)
			if (i > 0 && d < b.interval) || cap(b.snaps) == 0 {
				out = append(out, s)
				continue
			}
			b.snaps = b.snaps[0 : insertAt+1]
			if t := b.snaps[insertAt]; t != nil {
				overflow = append(overflow, t)
			}
			b.snaps[insertAt] = s
			insertAt++
			insertAt %= cap(b.snaps)
			prevCreated = s.created
		}
		in = overflow
		overflow = overflow[:0]
	}
	out = append(out, in...)
	return out
}

func (a *app) prune(p *profileJSON) error {
	snaps, err := findSnaps(*p.Storage)
	if err != nil {
		return err
	}
	out := a.cascade.insert(snaps)
	for _, s := range out {
		snapPath := path.Join(s.path, "snapshot")
		if _, err := os.Stat(snapPath); !os.IsNotExist(err) {
			// We're creating read-only subvolumes, which makes it
			// impossible for non-root-users to delete them. Since
			// we don't require to be run as root, unset the
			// read-only property.
			if err := a.btrfsCmd(
				"property",
				"set",
				"-t", "subvol",
				snapPath,
				"ro",
				"false",
			); err != nil {
				return err
			}
			// Delete the subvolume.
			if err := a.btrfsCmd(
				"subvolume",
				"delete",
				snapPath,
			); err != nil {
				return err
			}
		}
		if !a.opts.dryRun {
			if err := os.Remove(s.path); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *app) create(p *profileJSON) error {
	unixStr := strconv.FormatInt(time.Now().Unix(), 10)
	snapPath := path.Join("", *p.Storage, unixStr)
	if err := os.MkdirAll(snapPath, defaultDirMode); err != nil {
		return err
	}
	subvolPath := path.Join(snapPath, "/snapshot")
	return a.btrfsCmd(
		"subvolume",
		"snapshot",
		"-r",
		*p.Subvolume,
		subvolPath,
	)
}

type app struct {
	cfg     *configJSON
	cascade cascade
	opts    struct {
		btrfsBin    string
		cfgPath     string
		create      bool
		dryRun      bool
		list        bool
		profileName string
		prune       bool
		verbose     bool
	}
}

func (a *app) list(p *profileJSON) error {
	snaps, err := findSnaps(*p.Storage)
	if err != nil {
		return err
	}
	now := time.Now()
	for i, s := range snaps {
		delta := now.Sub(s.created)
		fmt.Printf("%8d\t%10s\t%s\n", i+1, ago(delta, 2), s.path)
	}
	return nil
}

func (a *app) btrfsCmd(args ...string) error {
	if a.opts.dryRun || a.opts.verbose {
		// TODO: Escape command-line arguments correctly not to
		//       produce confusing diagnostics.
		cmdline := []string{a.opts.btrfsBin}
		cmdline = append(cmdline, args...)
		fmt.Fprintln(os.Stderr, strings.Join(cmdline, " "))
	}
	if a.opts.dryRun {
		return nil
	}

	cmd := exec.Command(a.opts.btrfsBin, args...)
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	if err := cmd.Run(); err == nil {
		return nil
	} else if exitErr, ok := err.(*exec.ExitError); ok {
		stderr := "(stderr empty)"
		if stderrBuf.Len() > 0 {
			stderr = strings.Split(stderrBuf.String(), "\n")[0]
		}
		return fmt.Errorf("%s: failed with exit code %d: %s",
			a.opts.btrfsBin, exitErr.ExitCode(), stderr)
	} else {
		return err
	}
}

func (a *app) run() error {
	profileName := a.opts.profileName
	profile, ok := a.cfg.Profiles[profileName]
	if !ok {
		var knownNames []string
		for n := range a.cfg.Profiles {
			knownNames = append(knownNames, fmt.Sprintf("%q", n))
		}
		knownStr := strings.Join(knownNames, ", ")
		from := a.opts.cfgPath
		if fullCfgPath, err := filepath.Abs(from); err == nil {
			from = fullCfgPath
		}
		fmt.Fprintf(os.Stderr, "profile %q unknown, "+
			"known profiles are: %s (loaded from %s)\n",
			profileName, knownStr, from)
		os.Exit(1)
	}
	for _, b := range profile.Buckets {
		a.cascade.addBucket(b)
	}
	if a.opts.create {
		if err := a.create(profile); err != nil {
			return fmt.Errorf("cannot create snapshot: %w", err)
		}
	}
	if a.opts.prune {
		if err := a.prune(profile); err != nil {
			return fmt.Errorf("cannot prune snapshots: %w", err)
		}
	}
	if a.opts.list {
		if err := a.list(profile); err != nil {
			return fmt.Errorf("cannot list snapshots: %w", err)
		}
	}
	return nil
}

func main() {
	a := &app{}
	a.opts.cfgPath = "/etc/snap/config.json"
	var err error
	a.cfg, err = loadConfig(a.opts.cfgPath)
	if err != nil {
		panic(err)
	}
	a.cascade = newCascade()
	getopt.FlagLong(&a.opts.create, "create", 'c',
		"create a snapshot")
	getopt.FlagLong(&a.opts.dryRun, "dry-run", 0,
		"print what would be done, but don't do anything")
	getopt.FlagLong(&a.opts.list, "list", 'l',
		"list all snapshots")
	getopt.FlagLong(&a.opts.prune, "prune", 'X',
		"remove snapshots according to retention policy")
	getopt.FlagLong(&a.opts.verbose, "verbose", 'v',
		"explain what is being done")
	a.opts.btrfsBin = *getopt.StringLong("btrfs-bin", 'b', defaultBtrfsBin,
		"name of the btrfs binary (searched in $PATH)")
	getopt.SetParameters("profile-name")
	getopt.Parse()

	if getopt.NArgs() != 1 {
		fmt.Fprintln(os.Stderr, "profile-name argument missing")
		getopt.Usage()
		os.Exit(1)
	}
	a.opts.profileName = getopt.Arg(0)

	if err := a.run(); err != nil {
		fmt.Fprintf(os.Stderr, "TODO: %s\n", err.Error())
		os.Exit(1)
	}
}
