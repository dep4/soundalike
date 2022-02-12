// Copyright 2022 Daniel Erat.
// All rights reserved.

package main

import (
	"fmt"
	"log"
	"math/bits"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// scanOptions contains options for scanFiles.
type scanOptions struct {
	dir          string         // directory containing audio files
	fileString   string         // uncompiled fileRegexp
	fileRegexp   *regexp.Regexp // matches files to scan
	logSec       int            // logging frequency
	lookupThresh float64        // threshold for lookup table in (0.0, 1.0]
	matchThresh  float64        // threshold for song-to-song comparisons in (0.0, 1.0]
	skipBadFiles bool           // skip files that can't be fingerprinted by fpcalc
}

func defaultScanOptions() *scanOptions {
	return &scanOptions{
		// TODO: I'm just guessing what should be included here. See
		// https://en.wikipedia.org/wiki/Audio_file_format#List_of_formats and
		// https://en.wikipedia.org/wiki/FFmpeg#Supported_codecs_and_formats.
		fileString:   `\.(aiff|flac|m4a|mp3|oga|ogg|opus|wav|wma)$`,
		logSec:       10,
		lookupThresh: 0.25,
		matchThresh:  0.90,
		skipBadFiles: true,
	}
}

func (o *scanOptions) finish() error {
	o.dir = strings.TrimRight(o.dir, "/")
	if fi, err := os.Stat(o.dir); err != nil {
		return err
	} else if !fi.IsDir() {
		return fmt.Errorf("%v is not a directory", o.dir)
	}

	if o.lookupThresh <= 0 || o.lookupThresh > 1.0 {
		return fmt.Errorf("bad lookup threshold %v", o.lookupThresh)
	}
	if o.matchThresh <= 0 || o.matchThresh > 1.0 {
		return fmt.Errorf("bad match threshold %v", o.matchThresh)
	}

	var err error
	if o.fileRegexp, err = regexp.Compile(o.fileString); err != nil {
		return fmt.Errorf("bad file regexp: %v", err)
	}

	return nil
}

// scanFiles scans opts.dir and returns groups of similar files.
func scanFiles(opts *scanOptions, db *audioDB, fps *fpcalcSettings) ([][]*fileInfo, error) {
	lookup := newLookupTable()
	edges := make(map[fileID][]fileID)

	lastLog := time.Now()
	var scanned int
	if err := filepath.Walk(opts.dir, func(p string, fi os.FileInfo, err error) error {
		if p == opts.dir || fi.IsDir() || !opts.fileRegexp.MatchString(filepath.Base(p)) {
			return nil
		}

		rel := p[len(opts.dir)+1:]
		info, err := db.get(0, rel)
		if err != nil {
			return err
		} else if info == nil {
			finfo, err := runFpcalc(p, fps)
			if err != nil {
				if exit, ok := err.(*exec.ExitError); ok {
					stderr := strings.SplitN(string(exit.Stderr), "\n", 2)[0]
					if opts.skipBadFiles {
						log.Printf("Skipping %v: %v (%v)", p, err, stderr)
						return nil
					}
					return fmt.Errorf("%v: %v (%v)", p, err, stderr)
				}
				return err
			}
			info = &fileInfo{
				path:     rel,
				size:     fi.Size(),
				duration: finfo.Duration,
				fprint:   finfo.Fingerprint,
			}
			if info.id, err = db.save(info); err != nil {
				return err
			}
		}

		thresh := int(float64(len(info.fprint)) * opts.lookupThresh)
		for _, oid := range lookup.find(info.fprint, thresh) {
			oinfo, err := db.get(oid, "")
			if err != nil {
				return err
			} else if oinfo == nil {
				return fmt.Errorf("%d not in database", oid)
			}
			if score := compareFingerprints(info.fprint, oinfo.fprint); score >= opts.matchThresh {
				edges[info.id] = append(edges[info.id], oid)
				edges[oid] = append(edges[oid], info.id)
			}
		}

		lookup.add(info.id, info.fprint)

		scanned++
		if opts.logSec > 0 && time.Now().Sub(lastLog).Seconds() >= float64(opts.logSec) {
			log.Printf("Scanned %d files", scanned)
			lastLog = time.Now()
		}

		return nil
	}); err != nil {
		return nil, err
	}

	if opts.logSec > 0 {
		log.Printf("Finished scanning %d files", scanned)
	}

	var groups [][]*fileInfo
	for _, comp := range components(edges) {
		group := make([]*fileInfo, len(comp))
		for i, id := range comp {
			info, err := db.get(id, "")
			if err != nil {
				return nil, fmt.Errorf("getting info for %d: %v", id, err)
			} else if info == nil {
				return nil, fmt.Errorf("no info for %d", id)
			}
			group[i] = info
		}
		groups = append(groups, group)
	}
	return groups, nil
}

// compareFingerprints returns the ratio of identical bits in a and b
// to total bits in the longer of the two. All possible alignments are
// checked, and the highest ratio is returned.
func compareFingerprints(a, b []uint32) float64 {
	count := func(a, b []uint32) int {
		var cnt int
		for i := 0; i < len(a) && i < len(b); i++ {
			cnt += 32 - bits.OnesCount32(a[i]^b[i])
		}
		return cnt
	}

	best := count(a, b)
	for i := 1; i < len(a); i++ {
		if cnt := count(a[i:], b); cnt > best {
			best = cnt
		}
	}
	for i := 1; i < len(b); i++ {
		if cnt := count(a, b[i:]); cnt > best {
			best = cnt
		}
	}

	max := len(a)
	if len(b) > max {
		max = len(b)
	}
	return float64(best) / float64(32*max)
}

// components returns all components from the undirected graph described by edges.
func components(edges map[fileID][]fileID) [][]fileID {
	visited := make(map[fileID]struct{})

	var search func(fileID) []fileID
	search = func(src fileID) []fileID {
		if _, ok := visited[src]; ok {
			return nil
		}
		visited[src] = struct{}{}
		comp := []fileID{src}
		for _, dst := range edges[src] {
			comp = append(comp, search(dst)...)
		}
		return comp
	}

	var comps [][]fileID
	for src := range edges {
		if _, ok := visited[src]; !ok {
			comps = append(comps, search(src))
		}
	}
	return comps
}
