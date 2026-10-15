package ingest

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/AstRonin/radio-atlantida-backend/internal/media"
	"github.com/AstRonin/radio-atlantida-backend/internal/validation"
)

const (
	// maxFolders bounds the folder listing (docs/phase-18-plan.md,
	// decision 2).
	maxFolders = 500
	// maxFileListing bounds the file listing of one folder (decision 5).
	maxFileListing = 1000
	// maxTreeFiles bounds the walk behind the folder counts. A station has
	// about 800 files; the bound keeps a wrong MEDIA_ROOT (a whole disk)
	// from filling the memory.
	maxTreeFiles = 100_000
)

// FileStatus is what an import would do with a file (decision 5).
type FileStatus string

// File statuses.
const (
	// FileNew: an import would take the file.
	FileNew FileStatus = "new"
	// FileInLibrary: a track has this path.
	FileInLibrary FileStatus = "in_library"
	// FileOriginal: the original of a transcoded copy that a track plays
	// (decision 8).
	FileOriginal FileStatus = "original"
	// FileNotAudio: an import skips the file by its name.
	FileNotAudio FileStatus = "not_audio"
)

// Counts are the audio files of a folder and its subfolders.
type Counts struct {
	// AudioFiles are the files an import takes by name.
	AudioFiles int
	// NewFiles are the audio files that are not a track yet by path and
	// not the original of a transcoded copy.
	NewFiles int
}

// Folder is a folder under the media root with its counts.
type Folder struct {
	// Path is relative to the media root, with forward slashes.
	Path string
	Counts
}

// Folders are the folders under the media root.
type Folders struct {
	// Root counts the whole media root.
	Root    Counts
	Folders []Folder
	// More is true when the media root has more folders than listed.
	More bool
}

// FolderFile is a file of a folder with what an import would do with it.
type FolderFile struct {
	media.File
	Status FileStatus
	// TrackID is the track of the file (in_library) or of its copy
	// (original); 0 otherwise.
	TrackID uint64
}

// FolderFiles are the files of a folder and its subfolders.
type FolderFiles struct {
	Folder string
	Files  []FolderFile
	// More is true when the folder holds more files than listed.
	More bool
}

// Folders lists the folders under the media root, nested ones included,
// in path order, with their counts. The counts compare paths only; no
// file is read.
func (s *Service) Folders(ctx context.Context) (Folders, error) {
	tree, err := s.files.Tree(ctx, "", maxTreeFiles)
	if err != nil {
		return Folders{}, fmt.Errorf("list media folders: %w", err)
	}
	idx, err := s.libraryIndex(ctx, "")
	if err != nil {
		return Folders{}, err
	}
	counts := make(map[string]*Counts, len(tree.Folders))
	for _, f := range tree.Folders {
		counts[f] = &Counts{}
	}
	var out Folders
	for _, f := range tree.Files {
		st, _ := idx.status(f.Path)
		if st == FileNotAudio {
			continue
		}
		isNew := st == FileNew
		out.Root.add(isNew)
		for d := path.Dir(f.Path); d != "."; d = path.Dir(d) {
			if c := counts[d]; c != nil {
				c.add(isNew)
			}
		}
	}
	listed := tree.Folders
	if len(listed) > maxFolders {
		listed, out.More = listed[:maxFolders], true
	}
	out.Folders = make([]Folder, len(listed))
	for i, f := range listed {
		out.Folders[i] = Folder{Path: f, Counts: *counts[f]}
	}
	return out, nil
}

func (c *Counts) add(isNew bool) {
	c.AudioFiles++
	if isNew {
		c.NewFiles++
	}
}

// Files lists the files of folder ("" for the media root) and its
// subfolders, in path order, with what an import would do with each. An
// invalid or missing folder is refused per field (decision 9).
func (s *Service) Files(ctx context.Context, folder string) (FolderFiles, error) {
	errs := &validation.Error{}
	if err := s.checkFolder(folder, errs); err != nil {
		return FolderFiles{}, err
	}
	if err := errs.Err(); err != nil {
		return FolderFiles{}, err
	}
	files, more, err := s.files.Walk(ctx, folder, maxFileListing)
	if err != nil {
		return FolderFiles{}, fmt.Errorf("list folder: %w", err)
	}
	idx, err := s.libraryIndex(ctx, folder)
	if err != nil {
		return FolderFiles{}, err
	}
	out := FolderFiles{Folder: folder, Files: make([]FolderFile, len(files)), More: more}
	for i, f := range files {
		st, id := idx.status(f.Path)
		out.Files[i] = FolderFile{File: f, Status: st, TrackID: id}
	}
	return out, nil
}

// checkFolder adds the refusal of folder to errs (decision 9): a clean
// relative path, no hidden element, a directory under the media root. ""
// is the media root. An error is a failure to look.
func (s *Service) checkFolder(folder string, errs *validation.Error) error {
	if folder == "" {
		return nil
	}
	if err := media.ValidatePath(folder); err != nil {
		errs.Add("folder", validation.Invalid, "%s", strings.TrimPrefix(err.Error(), media.ErrInvalidPath.Error()+": "))
		return nil
	}
	for el := range strings.SplitSeq(folder, "/") {
		if strings.HasPrefix(el, ".") {
			errs.Add("folder", validation.Invalid, "hidden folders (a name starting with \".\") are not imported")
			return nil
		}
	}
	ok, err := s.files.IsDir(folder)
	if err != nil {
		return fmt.Errorf("check folder %s: %w", folder, err)
	}
	if !ok {
		errs.Add("folder", validation.Invalid, "no such folder in the media folder")
	}
	return nil
}

// libraryIndex reads the tracks under folder ("" for every track).
func (s *Service) libraryIndex(ctx context.Context, folder string) (libraryIndex, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	paths, err := s.store.TrackPaths(ctx, folder)
	if err != nil {
		return libraryIndex{}, fmt.Errorf("read track paths: %w", err)
	}
	return newLibraryIndex(paths), nil
}

// libraryIndex tells by path whether a file is a track or the original of
// a transcoded copy.
type libraryIndex struct {
	// paths maps a track's file path to the track.
	paths map[string]uint64
	// copies maps "<dir>/<stem>" of a track at "<dir>/<stem>.optimal.<ext>"
	// to that track.
	copies map[string]uint64
}

func newLibraryIndex(paths map[string]uint64) libraryIndex {
	idx := libraryIndex{paths: paths, copies: map[string]uint64{}}
	for p, id := range paths {
		stem, ok := strings.CutSuffix(strings.TrimSuffix(p, path.Ext(p)), optimalSuffix)
		if !ok || stem == "" || strings.HasSuffix(stem, "/") {
			continue
		}
		// Two copies of one original (a.optimal.mp3 and a.optimal.ogg):
		// the older track names it, whatever the map order.
		if old, taken := idx.copies[stem]; !taken || id < old {
			idx.copies[stem] = id
		}
	}
	return idx
}

// status returns what an import would do with the file at p and the track
// that decides it (decisions 5 and 8).
func (x libraryIndex) status(p string) (FileStatus, uint64) {
	if !importable(path.Base(p)) {
		return FileNotAudio, 0
	}
	if id, ok := x.paths[p]; ok {
		return FileInLibrary, id
	}
	// An original may have another extension than its copy (a FLAC
	// original, an MP3 copy).
	if id, ok := x.copies[strings.TrimSuffix(p, path.Ext(p))]; ok {
		return FileOriginal, id
	}
	return FileNew, 0
}
