package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"

	"github.com/dgraph-io/badger/v3"
	"github.com/spf13/pflag"
)

var debugflag, noresume *bool
var ignorelist = []string{
	// Compressed images
	"jpg",
	"jpeg",
	"png",
	"gif",
	"webp",
	// Compressed archive files
	"zip",
	"gz",
	"bz2",
	"xz",
	"7z",
	"rar",
	// Compressed video files
	"mp4",
	"avi",
	"mkv",
	"flv",
	"webm",
	// Compressed audio files
	"mp3",
	"wav",
	"ogg",
	"flac",
	// Other
	"pdf",
	"doc",
	"docx",
	"xls",
	"xlsx",
	"ppt",
	"pptx",
	"odt",
	"ods",
	"odp",
	"odg",
	"odf",
	"odc",
	"odm",
	"odt",
}

func log(format string, args ...interface{}) {
	if *debugflag {
		fmt.Printf(format+"\n", args...)
	}
}

func debug(format string, args ...interface{}) {
	if *debugflag {
		log(format, args...)
	}
}

func processfile(fp string, fi os.DirEntry, db *badger.DB) error {
	for _, suffix := range ignorelist {
		if strings.HasSuffix(strings.ToLower(fp), suffix) {
			// Skip
			debug("Skipping ignored file %s", fp)
			return nil
		}
	}

	fileinfo, err := fi.Info()
	if err != nil {
		return err
	}

	sysstat, ok := fileinfo.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("unknown file type %T", fileinfo.Sys())
	}

	if int64(sysstat.Blksize)*int64(sysstat.Blocks)*12 < int64(sysstat.Size)*10 { // If file is already compressed 1.2:1 then skip it
		// Already compressed or sparse, skip
		debug("Skipping already compressed or sparse file %s", fp)
		return nil
	}

	// See if the inode has been handled already
	var skip bool
	if db != nil {
		err = db.View(func(txn *badger.Txn) error {
			b := make([]byte, 8)
			binary.LittleEndian.PutUint64(b, uint64(sysstat.Ino))
			item, err := txn.Get(b)
			if err == nil {
				err = item.Value(func(val []byte) error {
					if string(val) == "handled" {
						debug("Skipping handled file %s", fp)
						skip = true
					}
					return nil
				})
			}
			if err == badger.ErrKeyNotFound {
				return nil
			}
			return err
		})
	}
	if err != nil {
		return err
	}
	if skip {
		return nil
	}

	// Process the file
	debug("Processing file %s", fp)

	source, err := os.Open(fp)
	if err != nil {
		return err
	}
	target, err := os.OpenFile(fp, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	// Copy from source to target
	copied, err := io.Copy(target, source)
	if err != nil {
		return err
	}
	target.Close()
	source.Close()

	if copied != sysstat.Size {
		return fmt.Errorf("copied %d bytes instead of %d", copied, sysstat.Size)
	}

	// Set the last modified timestamp to the original
	err = os.Chtimes(fp, fileinfo.ModTime(), fileinfo.ModTime())
	if err != nil {
		return err
	}

	// Start a write transaction.
	if db != nil {
		err = db.Update(func(txn *badger.Txn) error {
			// Set the key-value pair in the database.
			b := make([]byte, 8)
			binary.LittleEndian.PutUint64(b, uint64(sysstat.Ino))
			err := txn.Set(b, []byte("handled"))
			return err
		})
	}

	return err
}

func main() {
	ignore := pflag.String("ignore", strings.Join(ignorelist, ","), "Ignore files with these extensions")
	debugflag = pflag.Bool("debug", false, "Debug mode")
	noresume = pflag.Bool("noresume", false, "Dont create or use the resume database")
	pflag.Parse()

	ignorelist = []string{}
	for _, pattern := range strings.Split(*ignore, ",") {
		ignorelist = append(ignorelist, "."+strings.ToLower(pattern))
	}

	var db *badger.DB
	var err error

	if !*noresume {
		opts := badger.DefaultOptions(".zfs-inplace-recompress-resume")
		db, err = badger.Open(opts)
		if err != nil {
			log("Failed to open Badger resume database: %v", err)
			os.Exit(1)
		}
	}

	type queueItem struct {
		fp string
		fi os.DirEntry
	}

	filequeue := make(chan queueItem, runtime.NumCPU()*2)

	var abort bool

	// Ctrl-C handler to set abort
	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt)
		<-c
		log("Terminating, please wait for threads to finish tasks ...")
		abort = true
	}()

	var globalerror bool
	var workers sync.WaitGroup
	for i := 0; i < runtime.NumCPU(); i++ {
		workers.Add(1)
		go func() {
			for item := range filequeue {
				err := processfile(item.fp, item.fi, db)
				if err != nil {
					log("Error processing file %s: %v", item.fp, err)
					globalerror = true
				}
			}
			workers.Done()
		}()
	}

	err = filepath.WalkDir(".", func(fp string, fi os.DirEntry, err error) error {
		if globalerror {
			return errors.New("Aborted due to global error")
		}
		if abort {
			return errors.New("Aborted due to interrupt")
		}

		if err != nil {
			log("Error walking directory: %v", err)
			return nil // but continue walking elsewhere
		}

		if fi.Type().IsRegular() {
			// Find the file inode
			filequeue <- queueItem{fp, fi}
		}
		return nil
	})

	close(filequeue)
	workers.Wait()

	if db != nil {
		db.Close()
	}

	if err != nil {
		log("Error walking directory: %v", err)
		os.Exit(1)
	} else {
		if !*noresume {
			os.RemoveAll(".zfs-inplace-recompress-resume")
		}
	}
}
