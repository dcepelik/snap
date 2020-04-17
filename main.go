package main

import (
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pborman/getopt/v2"
)

const defaultDirPerm = 0755

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
		path := path.Join(dir, fi.Name())
		createdUnix, err := strconv.ParseInt(fi.Name(), 10, 64)
		if err != nil {
			return nil, err
		}
		created := time.Unix(createdUnix, 0)
		snaps = append(snaps, &snap{path, created})
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
			interval := s.created.Sub(prevCreated)
			if i > 0 && interval < b.interval {
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

const (
	day   = 24 * time.Hour
	week  = 7 * day
	month = 30 * day
)

func (a *app) prune(p *profileJSON) error {
	snaps, err := findSnaps(*p.Storage)
	if err != nil {
		return err
	}
	out := a.cascade.insert(snaps)
	for _, s := range out {
		fmt.Println("btrfs", "subvolume", "delete", s.path)
	}
	return nil
}

func (a *app) create(p *profileJSON) error {
	unixStr := strconv.FormatInt(time.Now().Unix(), 10)
	path := path.Join("", *p.Storage, unixStr)
	fmt.Println("btrfs", "subvolume", "snapshot", *p.Subvolume, path)
	if err := os.MkdirAll(path, defaultDirPerm); err != nil {
		return err
	}
	return nil
}

type app struct {
	cfg     *configJSON
	cascade cascade
	opts    struct {
		create      bool
		list        bool
		dryRun      bool
		verbose     bool
		prune       bool
		profileName string
		cfgPath     string
	}
}

func (a *app) list(p *profileJSON) error {
	snaps, err := findSnaps(*p.Storage)
	if err != nil {
		return err
	}
	for i, s := range snaps {
		fmt.Println(i+1, s.path, s.created)
	}
	return nil
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
	a.opts.cfgPath = "config.json"
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
	getopt.SetParameters("profile-name")
	getopt.Parse()

	if getopt.NArgs() != 1 {
		fmt.Fprintln(os.Stderr, "profile-name argument missing")
		getopt.Usage()
		os.Exit(1)
	}
	a.opts.profileName = getopt.Arg(0)

	if err := a.run(); err != nil {
		fmt.Fprintln(os.Stderr, "TODO: %v", err)
		os.Exit(1)
	}
}
