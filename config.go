package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"
)

type ProfileName = string

type BucketInterval time.Duration

func (d *BucketInterval) UnmarshalText(text []byte) (err error) {
	s := string(text)
	defer func() {
		if err != nil {
			err = fmt.Errorf("invalid interval %q: %w", s, err)
		}
	}()
	if len(s) == 0 {
		return fmt.Errorf("cannot be blank")
	}
	l := len(s) - 1
	ns, unit := s[0:l], s[l]
	n, err := strconv.Atoi(ns)
	if err != nil {
		return
	}
	const day = 24 * time.Hour
	const week = 7 * day
	const month = 30 * day
	const year = 365 * day
	nd := time.Duration(n)
	switch unit {
	case 's':
		*d = BucketInterval(nd * time.Second)
	case 'm':
		*d = BucketInterval(nd * time.Minute)
	case 'h':
		*d = BucketInterval(nd * time.Hour)
	case 'd':
		*d = BucketInterval(nd * day)
	case 'w':
		*d = BucketInterval(nd * week)
	case 'M':
		*d = BucketInterval(nd * month)
	case 'y':
		*d = BucketInterval(nd * year)
	default:
		return fmt.Errorf("invalid unit: %q", unit)
	}
	return
}

type configJSON struct {
	Profiles map[ProfileName]*profileJSON
}

func (c *configJSON) validate() error {
	for name, p := range c.Profiles {
		if err := p.validate(); err != nil {
			return fmt.Errorf("profile %q: %w", name, err)
		}
	}
	return nil
}

type profileJSON struct {
	Subvolume *string
	Storage   *string
	Buckets   []*bucketJSON
}

func (p *profileJSON) validate() error {
	if p.Subvolume == nil {
		return fmt.Errorf("profile %q: Subvolume missing")
	}
	for i, b := range p.Buckets {
		if err := b.validate(); err != nil {
			l := len(p.Buckets)
			return fmt.Errorf("bucket #%d/%d: %w", i+1, l, err)
		}
	}
	return nil
}

type bucketJSON struct {
	Interval *BucketInterval
	Size    *int
}

func (b *bucketJSON) validate() error {
	if b.Interval == nil {
		return fmt.Errorf("Interval is missing")
	}
	if b.Size == nil {
		return fmt.Errorf("Size is missing")
	}
	return nil
}

func loadConfig(filename string) (*configJSON, error) {
	f, err := os.Open("config.json")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var cfg configJSON
	if err := json.NewDecoder(f).Decode(&cfg); err != nil {
		return nil, err
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}
