package main

import (
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/skeema/mycli"
	"github.com/skeema/tengo"
)

type Dir struct {
	Path    string
	Config  *mycli.Config // Unified config including this dir's options file (and its parents' open files)
	section string        // For options files, which section name to use, if any
}

// NewDir returns a value representing a directory that Skeema may operate upon.
// This function should be used when initializing a dir when we aren't directly
// operating on any of its parent dirs.
// path may be either an absolute or relative directory path.
// baseConfig should only include "global" configurations; any config files in
// parent dirs will automatically be read in and cascade appropriately into this
// directory's config.
func NewDir(path string, baseConfig *mycli.Config) (*Dir, error) {
	cleanPath, err := filepath.Abs(filepath.Clean(path))
	if err == nil {
		path = cleanPath
	}

	dir := &Dir{
		Path:    path,
		Config:  baseConfig.Clone(),
		section: baseConfig.Get("environment"),
	}

	// Get slice of option files from root on down to this dir, in that order.
	// Then parse and apply them to the config.
	dirOptionFiles, err := dir.cascadingOptionFiles()
	if err != nil {
		return nil, err
	}
	for _, optionFile := range dirOptionFiles {
		err := optionFile.Parse(dir.Config)
		if err != nil {
			return nil, err
		}
		_ = optionFile.UseSection(dir.section) // we don't care if the section doesn't exist
		dir.Config.AddSource(optionFile)
	}

	return dir, nil
}

func (dir *Dir) String() string {
	return dir.Path
}

func (dir *Dir) CreateIfMissing() (created bool, err error) {
	fi, err := os.Stat(dir.Path)
	if err == nil {
		if !fi.IsDir() {
			return false, fmt.Errorf("Path %s already exists but is not a directory", dir.Path)
		}
		return false, nil
	}
	if !os.IsNotExist(err) {
		return false, fmt.Errorf("Unable to use directory %s: %s\n", dir.Path, err)
	}
	err = os.MkdirAll(dir.Path, 0777)
	if err != nil {
		return false, fmt.Errorf("Unable to create directory %s: %s\n", dir.Path, err)
	}
	return true, nil
}

func (dir *Dir) Delete() error {
	return os.RemoveAll(dir.Path)
}

func (dir *Dir) HasFile(name string) bool {
	_, err := os.Stat(path.Join(dir.Path, name))
	return (err == nil)
}

func (dir *Dir) HasOptionFile() bool {
	return dir.HasFile(".skeema")
}

func (dir *Dir) HasHost() bool {
	return dir.Config.Changed("host")
}

func (dir *Dir) HasSchema() bool {
	return dir.Config.Changed("schema")
}

// InstanceKey returns a string usable for grouping directories by what database
// instances they will target.
func (dir *Dir) InstanceKey() string {
	if !dir.Config.Changed("host") {
		return ""
	}
	host := dir.Config.Get("host")

	// TODO: support cloudsql
	if host == "localhost" && (dir.Config.Changed("socket") || !dir.Config.Changed("port")) {
		return fmt.Sprintf("%s:%s", host, dir.Config.Get("socket"))
	}
	return fmt.Sprintf("%s:%d", host, dir.Config.GetIntOrDefault("port"))
}

// FirstInstance returns at most one tengo.Instance based on the directory's
// configuration. If the config maps to multiple instances (NOT YET SUPPORTED)
// only the first will be returned. If the config maps to no instances, nil
// will be returned.
func (dir *Dir) FirstInstance() (*tengo.Instance, error) {
	if !dir.HasHost() {
		return nil, nil
	}

	var userAndPass string
	if !dir.Config.Changed("password") {
		userAndPass = dir.Config.Get("user")
	} else {
		userAndPass = fmt.Sprintf("%s:%s", dir.Config.Get("user"), dir.Config.Get("password"))
	}

	// Construct DSN using either Unix domain socket or tcp/ip host and port
	params := "interpolateParams=true&foreign_key_checks=0"
	var dsn string
	if dir.Config.Get("host") == "localhost" && (dir.Config.Changed("socket") || !dir.Config.Changed("port")) {
		dsn = fmt.Sprintf("%s@unix(%s)/?%s", userAndPass, dir.Config.Get("socket"), params)
	} else {
		// TODO support host configs mapping to multiple lookups via service discovery
		dsn = fmt.Sprintf("%s@tcp(%s:%d)/?%s", userAndPass, dir.Config.Get("host"), dir.Config.GetIntOrDefault("port"), params)
	}
	// TODO also support cloudsql

	// TODO support drivers being overriden
	driver := "mysql"

	instance, err := tengo.NewInstance(driver, dsn)
	if err != nil || instance == nil {
		if dir.Config.Changed("password") {
			safeUserPass := fmt.Sprintf("%s:*****", dir.Config.Get("user"))
			dsn = strings.Replace(dsn, userAndPass, safeUserPass, 1)
		}
		return nil, fmt.Errorf("Invalid connection information for %s (DSN=%s): %s", dir, dsn, err)
	}
	if ok, err := instance.CanConnect(); !ok {
		return nil, fmt.Errorf("Unable to connect to %s for %s: %s", instance, dir, err)
	}
	return instance, nil
}

// SQLFiles returns a slice of SQLFile pointers, representing the valid *.sql
// files that already exist in a directory. Does not recursively search
// subdirs.
// An error will only be returned if we are unable to read the directory.
// This method attempts to call Read() on each SQLFile to populate it; per-file
// read errors are tracked within each SQLFile struct.
func (dir *Dir) SQLFiles() ([]*SQLFile, error) {
	fileInfos, err := ioutil.ReadDir(dir.Path)
	if err != nil {
		return nil, err
	}
	result := make([]*SQLFile, 0, len(fileInfos))
	for _, fi := range fileInfos {
		sf := &SQLFile{
			Dir:      dir,
			FileName: fi.Name(),
			fileInfo: fi,
		}
		if sf.ValidatePath(true) == nil {
			sf.Read()
			result = append(result, sf)
		}
	}

	return result, nil
}

// Subdirs returns a slice of direct subdirectories of the current dir. An
// error will be returned if there are problems reading the directory list.
// If the subdirectory has an option file, it will be read and parsed, with
// any errors in either step proving fatal.
func (dir *Dir) Subdirs() ([]*Dir, error) {
	fileInfos, err := ioutil.ReadDir(dir.Path)
	if err != nil {
		return nil, err
	}
	result := make([]*Dir, 0, len(fileInfos))
	for _, fi := range fileInfos {
		if fi.IsDir() {
			subdir := &Dir{
				Path:    path.Join(dir.Path, fi.Name()),
				Config:  dir.Config.Clone(),
				section: dir.section,
			}
			if subdir.HasOptionFile() {
				f, err := subdir.OptionFile()
				if err != nil {
					return nil, err
				}
				err = f.Parse(subdir.Config)
				if err != nil {
					return nil, err
				}
				_ = f.UseSection(subdir.section) // we don't care if the section doesn't exist
				subdir.Config.AddSource(f)
			}
			result = append(result, subdir)
		}
	}
	return result, nil
}

// Subdir creates and returns a new subdir of the current dir.
func (dir *Dir) CreateSubdir(name string, optionFile *mycli.File) (*Dir, error) {
	subdir := &Dir{
		Path:    path.Join(dir.Path, name),
		Config:  dir.Config.Clone(),
		section: dir.section,
	}

	if created, err := subdir.CreateIfMissing(); err != nil {
		return nil, err
	} else if !created {
		return nil, fmt.Errorf("Directory %s already exists", subdir)
	}

	if optionFile != nil {
		if err := subdir.CreateOptionFile(optionFile); err != nil {
			return nil, err
		}
	}
	return subdir, nil
}

func (dir *Dir) CreateOptionFile(optionFile *mycli.File) error {
	optionFile.Dir = dir.Path
	if err := optionFile.Write(false); err != nil {
		return fmt.Errorf("Unable to write to %s: %s", optionFile.Path(), err)
	}
	_ = optionFile.UseSection(dir.section)
	dir.Config.AddSource(optionFile)
	return nil
}

func (dir *Dir) Targets(expandInstances, expandSchemas bool) <-chan Target {
	targets := make(chan Target)
	go func() {
		generateTargetsForDir(dir, targets, expandInstances, expandSchemas)
		close(targets)
	}()
	return targets
}

// OptionFile returns a pointer to a mycli.File for this directory, representing
// the dir's .skeema file, if one exists. The file will be read but not parsed.
func (dir *Dir) OptionFile() (*mycli.File, error) {
	f := mycli.NewFile(dir.Path, ".skeema")
	if err := f.Read(); err != nil {
		return nil, err
	}
	return f, nil
}

// cascadingOptionFiles returns a slice of *mycli.File, corresponding to the
// option file in this dir as well as its parent dir hierarchy. Evaluation
// of parent dirs stops once we hit either a directory containing .git, the
// user's home directory, or the root of the filesystem. The result is ordered
// such that the closest-to-root dir's File is returned first and this dir's
// File last. The files will be read, but not parsed.
func (dir *Dir) cascadingOptionFiles() (files []*mycli.File, errReturn error) {
	home := filepath.Clean(os.Getenv("HOME"))

	// we know the first character will be a /, so discard the first split result
	// which we know will be an empty string
	components := strings.Split(dir.Path, string(os.PathSeparator))[1:]
	files = make([]*mycli.File, 0, len(components))

	// Examine parent dirs, going up one level at a time, stopping early if we
	// hit either the user's home directory or a directory containing a .git subdir.
	base := 0
	for n := len(components) - 1; n >= 0 && base == 0; n-- {
		curPath := "/" + path.Join(components[0:n+1]...)
		if curPath == home {
			base = n
		}
		fileInfos, err := ioutil.ReadDir(curPath)
		// We ignore errors here since we expect the dir to not exist in some cases
		// (for example, init command on a new dir)
		if err != nil {
			continue
		}
		for _, fi := range fileInfos {
			if fi.Name() == ".git" {
				base = n
			} else if fi.Name() == ".skeema" {
				f := mycli.NewFile(curPath, ".skeema")
				if readErr := f.Read(); readErr != nil {
					errReturn = readErr
				} else {
					files = append(files, f)
				}
			}
		}
	}

	// Reverse the order of the result, so that dir's option file is last. This way
	// we can easily add the files to the config by applying them in order.
	for left, right := 0, len(files)-1; left < right; left, right = left+1, right-1 {
		files[left], files[right] = files[right], files[left]
	}
	return
}
