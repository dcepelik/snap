package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pborman/getopt/v2"
	"golang.org/x/sync/errgroup"
)

const (
	defaultDirMode     = 0755
	defaultBtrfsBin    = "btrfs"
	defaultConfigPath  = "/etc/snap/config.json"
	snapshotDateLayout = "2006-01-02_15:04:05"
)

type snap struct {
	path, subvolPath string
	created          time.Time
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
		created, err := time.Parse(snapshotDateLayout, fi.Name())
		if err != nil {
			continue
		}
		// FIXME: This way of "detecting" subvolumes sucks. It would be
		//     way better to just use `btrfs subvolume list`, but that
		//     requires SYS_CAP_ADMIN. Is this mess worth being able
		//     to run snap as regular user?
		subvolPath := path.Join(snapPath, "snapshot")
		if sfi, err := os.Lstat(subvolPath); err != nil || !sfi.IsDir() {
			continue
		}
		snaps = append(snaps, &snap{snapPath, subvolPath, created})
	}
	return snaps, nil
}

type app struct {
	cfg     *configJSON
	cascade cascade
	opts    struct {
		backup      bool
		btrfsBin    *string
		cfgPath     *string
		create      bool
		dryRun      bool
		list        bool
		listFiles   *string
		profileName string
		prune       bool
		verbose     bool
	}
}

type fileBackup struct {
	Name    string
	Dir     string
	Size    int64
	ModTime time.Time
	Mode    fs.FileMode
}

func (a fileBackup) Less(b *fileBackup) bool {
	if a.Name == b.Name {
		return a.ModTime.Before(b.ModTime)
	}
	return a.Name < b.Name
}

func (a *app) listFiles(p *profileJSON) error {
	path := *a.opts.listFiles
	if !filepath.IsAbs(path) {
		pwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("os.Getpwd: %w", err)
		}
		path = filepath.Join(pwd, path)
	}
	snaps, err := findSnaps(*p.Storage)
	if err != nil {
		return err
	}
	backups := make(map[fileBackup]*snap)
	for _, s := range snaps {
		backupPath := filepath.Join(s.subvolPath, path)
		fi, err := os.Stat(backupPath)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		fis := make([]fs.FileInfo, 0, 1)
		var dir string
		if fi.IsDir() {
			dir = path
			des, err := os.ReadDir(backupPath)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			for _, de := range des {
				fi, err := de.Info()
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				fis = append(fis, fi)
			}
		} else {
			dir = filepath.Dir(path)
			fis = append(fis, fi)
		}
		for _, fi := range fis {
			if fi.IsDir() {
				continue
			}
			backups[fileBackup{
				Name:    filepath.Join(dir, fi.Name()),
				Size:    fi.Size(),
				ModTime: fi.ModTime(),
				Mode:    fi.Mode(),
			}] = s
		}
	}
	byName := make([]*fileBackup, 0, len(backups))
	for b := range backups {
		tmp := b
		byName = append(byName, &tmp)
	}
	sort.Slice(byName, func(i, j int) bool { return byName[i].Less(byName[j]) })
	for _, b := range byName {
		s := backups[*b]
		fullPath := filepath.Join(s.subvolPath, b.Dir, b.Name)
		fmt.Printf("%11s\t%10d\t%-8s\t%s\n",
			b.Mode.String(),
			b.Size,
			agoR(time.Since(b.ModTime), 2),
			fullPath,
		)
	}
	return nil
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
	if p.Subvolume == nil {
		return errors.New("this is a backup profile")
	}
	snapPath := path.Join("", *p.Storage, time.Now().UTC().Format(snapshotDateLayout))
	if err := os.MkdirAll(snapPath, defaultDirMode); err != nil {
		return err
	}
	subvolPath := path.Join(snapPath, "/snapshot")
	// Snapshots are atomic. This either fails or a snapshot gets created.
	// No cleanup is required. But if the snapshot isn't created, the
	// os.Remove below will remove the empty directory afterwards.
	defer os.Remove(snapPath)
	return a.btrfsCmd(
		"subvolume",
		"snapshot",
		"-r",
		*p.Subvolume,
		subvolPath,
	)
}

// TODO: Implement dry run.
func (a *app) backup(p *profileJSON) error {
	if p.Backup == nil {
		return errors.New("this is not a backup profile")
	}

	srcProfile := a.cfg.Profiles[*p.Backup]
	if srcProfile == nil {
		panic("FIXME") // FIXME
	}
	src, err := findSnaps(*srcProfile.Storage)
	if err != nil {
		return err
	}
	dst, err := findSnaps(*p.Storage)
	if err != nil {
		return err
	}

	srcM := make(map[time.Time]*snap, len(src))
	for _, s := range src {
		srcM[s.created] = s
	}
	dstM := make(map[time.Time]*snap, len(dst))
	for _, s := range dst {
		dstM[s.created] = s
	}

	// Calculate which snapshots we need to backup ("need", present in src,
	// but not in dst) and which ones we can use as bases for incremental
	// backup ("have", are both in src and in dst).
	need := make(map[time.Time]*snap)
	have := make(map[time.Time]*snap)
	for st, ss := range srcM {
		if _, ok := dstM[st]; !ok {
			need[st] = ss
		} else {
			have[st] = ss
		}
	}

	needS := make([]*snap, 0, len(need))
	for _, s := range need {
		needS = append(needS, s)
	}
	sort.Slice(needS, func(i, j int) bool {
		return needS[i].created.Before(needS[j].created)
	})

	// Figure out snapshots are unwanted (= would be deleted by next prune
	// operation = do not the retention policy of the target). Those we
	// will not back up.
	wouldHave := append(dst, needS...)
	unwanted := make(map[time.Time]*snap)
	for _, s := range a.cascade.insert(wouldHave) {
		unwanted[s.created] = s
	}
	a.cascade.reset()

	haveS := make([]*snap, 0, len(have)+len(need))
	for _, s := range have {
		haveS = append(haveS, s)
	}
	sort.Slice(haveS, func(i, j int) bool {
		return haveS[i].created.Before(haveS[j].created)
	})

	for _, s := range needS {
		if _, ok := unwanted[s.created]; ok {
			continue
		}
		if err := a.backupSingle(srcProfile, p, s, haveS); err != nil {
			return fmt.Errorf("cannot backup %s: %w", s.path, err)
		}
		haveS = append(haveS, s)
	}
	return nil
}

func (a *app) backupSingle(srcP, dstP *profileJSON, s *snap, have []*snap) error {
	name := s.created.Format(snapshotDateLayout)
	snapPath := path.Join("", *dstP.Storage, name)

	startAndWait := func(cmd *exec.Cmd, name string) error {
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Start(); err != nil {
			return err
		}
		if err := cmd.Wait(); err != nil {
			raw := strings.TrimSpace(stderr.String())
			if len(raw) == 0 {
				return err
			}
			lines := strings.Split(raw, "\n")
			sample := lines[0]
			if n := len(lines); n > 1 {
				sample += fmt.Sprintf(" [%d more lines...]", n-1)
			}
			return fmt.Errorf("%s: %w (stderr: %q)", name, err, sample)
		}
		return nil
	}

	pr, pw := io.Pipe()
	g, ctx := errgroup.WithContext(context.Background())

	// btrfs receive ...
	recvPath, err := os.MkdirTemp(*dstP.Storage, name+".recv.*")
	_ = os.Chmod(recvPath, defaultDirMode)
	defer os.Remove(recvPath)
	if err != nil {
		return fmt.Errorf("os.MkdirTemp: %w", err)
	}
	recvArgs := []string{"receive", recvPath}
	recv := exec.CommandContext(ctx, *a.opts.btrfsBin, recvArgs...)
	recv.Stdin = pr

	// btrfs send ...
	sendArgs := []string{"send"}
	var parentPath string
	for _, hs := range have {
		if hs.created.Before(s.created) {
			parentPath = hs.subvolPath
		}
		sendArgs = append(sendArgs, "-c")
		sendArgs = append(sendArgs, hs.subvolPath)
	}
	if parentPath != "" {
		sendArgs = append(sendArgs, "-p")
		sendArgs = append(sendArgs, parentPath)
	}
	sendArgs = append(sendArgs, s.subvolPath)
	send := exec.CommandContext(ctx, *a.opts.btrfsBin, sendArgs...)
	send.Stdout = pw

	if a.opts.dryRun || a.opts.verbose {
		cmdline := []string{"btrfs"}
		cmdline = append(cmdline, escapedArgs(sendArgs, 3)...)
		cmdline = append(cmdline, "|", "btrfs")
		cmdline = append(cmdline, escapedArgs(recvArgs, 3)...)
		fmt.Fprintln(os.Stderr, strings.Join(cmdline, " "))
	}
	if a.opts.dryRun {
		return nil
	}

	g.Go(func() error {
		defer pr.Close()
		return startAndWait(recv, "btrfs receive")
	})
	g.Go(func() error {
		defer pw.Close()
		return startAndWait(send, "btrfs send")
	})
	if err := g.Wait(); err != nil {
		return err
	}
	os.Remove(snapPath) // Remove any previous (unused) snapshot directory
	return os.Rename(recvPath, snapPath)
}

func (a *app) list(p *profileJSON) error {
	snaps, err := findSnaps(*p.Storage)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for i, s := range snaps {
		delta := now.Sub(s.created)
		fmt.Printf("%8d\t%10s\t%s\n", i+1, ago(delta, 2), s.path)
	}
	return nil
}

func escapedArgs(args []string, max int) []string {
	maxArgs := args
	if max > 0 && len(args) > max {
		maxArgs = append([]string{args[0], "..."}, args[len(args)-max:]...)
	}
	escapedArgs := make([]string, len(maxArgs))
	for i, a := range maxArgs {
		var escape bool
	loop:
		for _, r := range a {
			switch {
			case r == '-':
			case r == '.':
			case r == '/':
			case r == '@':
			case r == '_':
			case r >= '0' && r <= '9':
			case r >= 'A' && r <= 'Z':
			case r >= 'a' && r <= 'z':
			default:
				escape = true
				break loop
			}
		}
		escapedArgs[i] = a
		if escape {
			escapedArgs[i] = fmt.Sprintf("%q", escapedArgs[i])
		}
	}
	return escapedArgs
}

func (a *app) btrfsCmd(args ...string) error {
	if a.opts.dryRun || a.opts.verbose {
		argsStr := strings.Join(escapedArgs(args, 10), ", ")
		fmt.Fprintf(os.Stderr, "exec.Command(%s)\n", argsStr)
	}
	if a.opts.dryRun {
		return nil
	}

	cmd := exec.Command(*a.opts.btrfsBin, args...)
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	err := cmd.Run()
	if exitErr, ok := err.(*exec.ExitError); ok {
		stderr := "(stderr empty)"
		if stderrBuf.Len() > 0 {
			stderr = strings.Split(stderrBuf.String(), "\n")[0]
		}
		return fmt.Errorf("%s: failed with exit code %d: %s",
			*a.opts.btrfsBin, exitErr.ExitCode(), stderr)
	} else if err != nil {
		return err
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
		from := *a.opts.cfgPath
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
	if a.opts.backup {
		if err := a.backup(profile); err != nil {
			return fmt.Errorf("cannot backup profile: %w", err)
		}
	}
	a.cascade = newCascade()
	for _, b := range profile.Buckets {
		a.cascade.addBucket(b)
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
	if a.opts.listFiles != nil {
		if err := a.listFiles(profile); err != nil {
			return fmt.Errorf("cannot listFiles files: %w", err)
		}
	}
	return nil
}

func main() {
	a := &app{}
	a.opts.cfgPath = getopt.StringLong("config", 'C', defaultConfigPath,
		"path to config file")
	a.opts.btrfsBin = getopt.StringLong("btrfs-bin", 'B', defaultBtrfsBin,
		"name of the btrfs binary (searched in $PATH)")
	getopt.FlagLong(&a.opts.backup, "backup", 'b',
		"backup snapshots")
	getopt.FlagLong(&a.opts.create, "create", 'c',
		"create a snapshot")
	getopt.FlagLong(&a.opts.dryRun, "dry-run", 0,
		"print what would be done, but don't do anything")
	getopt.FlagLong(&a.opts.list, "list", 'l',
		"list all snapshots")
	getopt.FlagLong(&a.opts.verbose, "verbose", 'v',
		"print what is being done")
	getopt.FlagLong(&a.opts.prune, "prune", 'X',
		"remove snapshots according to retention policy")
	a.opts.listFiles = getopt.StringLong("list-files", 'L',
		"list all distinct backups of files in a given directory")
	getopt.SetParameters("profile")
	getopt.Parse()

	var err error
	a.cfg, err = loadConfig(*a.opts.cfgPath)
	if err != nil {
		panic(err)
	}
	a.cascade = newCascade()
	if getopt.NArgs() != 1 {
		fmt.Fprintln(os.Stderr, "profile-name argument missing")
		getopt.Usage()
		os.Exit(1)
	}
	a.opts.profileName = getopt.Arg(0)

	if err := a.run(); err != nil {
		fmt.Fprintf(os.Stderr, "snap: %s: %s\n", a.opts.profileName, err.Error())
		os.Exit(1)
	}
}
