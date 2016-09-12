package main

import (
	"crypto/md5"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"github.com/deckarep/golang-set"
	"github.com/kuba--/xattr"
	"github.com/ushis/m3u"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
	"gopkg.in/cheggaaa/pb.v1"

	"gitlab.kohlby.fr/adrienkohlbecker/ci/utils/errors"
)

var playlistsToExport = []string{"BEST", "GOOGLE PLAY", "DNB", "TRANCE", "ELECTRONNIE", "ABOU", "COLINE", "ELECTROSYLVESTRE", "STARRED", "GRATTE", "PARTY"}

const itunesMusicDir = "/Users/adrien/Music/iTunes/iTunes Media/Music"
const destFolder = "/Users/adrien/Dropbox/Music"
const hashXattrName = "fr.kohlby.m3u:hash"

var forbiddenCharsRegexp = regexp.MustCompile("[^a-zA-Z0-9\\.\\/ ]")
var mp4InfoRegexp = regexp.MustCompile("(?s)audio\\s+(.*), (\\d+\\.\\d+) secs.*Name: ([^\\n]*)\\n[^\\n]*Artist: ([^\\n]*)")

type codec int

const (
	codecUnknown codec = iota
	codecMP3
	codecAAC
	codecALAC
)

func fatal(err errors.Error) {
	fmt.Println(err.ErrorStack())
	os.Exit(1)
}

func main() {

	log.Println("Exporting playlists from iTunes...")

	m3uPath, err := createTemporaryDirectory()
	if err != nil {
		fatal(err)
	}
	defer os.RemoveAll(m3uPath)

	err = exportPlaylistsFromItunes(m3uPath)
	if err != nil {
		fatal(err)
	}

	playlistFiles, err := listPlaylistFiles(m3uPath)
	if err != nil {
		fatal(err)
	}

	playlists, err := readPlaylists(playlistFiles)
	if err != nil {
		fatal(err)
	}

	log.Println("done")

	uniqueTracks := buildTrackSet(playlists)

	log.Println("Reading source tracks metadata...")

	metadatas, err := readTracksMetadata(uniqueTracks)
	if err != nil {
		fatal(err)
	}

	log.Println("done")
	log.Println("Copying tracks...")

	err = ensureFolderExists(destFolder)
	if err != nil {
		fatal(err)
	}

	err = copyTracks(destFolder, metadatas)
	if err != nil {
		fatal(err)
	}

	log.Println("done")
	log.Println("Writing playlists...")

	err = exportPlaylists(destFolder, playlists, metadatas)
	if err != nil {
		fatal(err)
	}

	log.Println("done")

}

func createTemporaryDirectory() (string, errors.Error) {

	m3uPath, err := ioutil.TempDir(os.TempDir(), "m3u")
	if err != nil {
		return "", errors.Wrap(err, 0)
	}
	return m3uPath, nil

}

func exportPlaylistsFromItunes(exportDirectory string) errors.Error {

	cmd := exec.Command(
		"java",
		"-jar",
		"./vendor/iTunesExportScala-2.2.2/itunesexport.jar",
		"-fileTypes=ALL",
		fmt.Sprintf("-includePlaylist=%s", strings.Join(playlistsToExport, ",")),
		fmt.Sprintf("-outputDir=%s", exportDirectory),
	)
	err := cmd.Run()
	if err != nil {
		return errors.Wrap(err, 0)
	}

	return nil

}

func listPlaylistFiles(exportDirectory string) ([]string, errors.Error) {

	paths := []string{}

	err := filepath.Walk(exportDirectory, func(path string, info os.FileInfo, walkErr error) error {

		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".m3u" {
			return nil
		}
		paths = append(paths, path)
		return nil

	})
	if err != nil {
		return paths, errors.Wrap(err, 0)
	}

	return paths, nil

}

func readPlaylists(playlistFiles []string) (map[string]m3u.Playlist, errors.Error) {

	playlists := make(map[string]m3u.Playlist)

	for _, path := range playlistFiles {

		pl, err := readPlaylist(path)
		if err != nil {
			return playlists, err
		}
		name := filepath.Base(path)

		playlists[name] = pl

	}

	return playlists, nil

}

func readPlaylist(path string) (m3u.Playlist, errors.Error) {

	f, err := os.Open(path)
	if err != nil {
		return nil, errors.Wrap(err, 0)
	}
	defer f.Close()

	pl, err := m3u.Parse(f)
	if err != nil {
		return nil, errors.Wrap(err, 0)
	}

	for i, track := range pl {
		track.Path = strings.Replace(track.Path, "file://", "", 1)
		pl[i] = track
	}

	return pl, nil

}

func buildTrackSet(playlists map[string]m3u.Playlist) mapset.Set {

	uniqueTracks := mapset.NewSet()
	for _, pl := range playlists {
		for _, track := range pl {
			uniqueTracks.Add(track.Path)
		}
	}

	return uniqueTracks

}

type trackMetadata struct {
	OriginalPath string
	CleanedPath  string
	Artist       string
	Name         string
	Length       float64
	Hash         string
	Codec        codec
}

func readTracksMetadata(tracks mapset.Set) (map[string]*trackMetadata, errors.Error) {

	tracksMetadata := make(map[string]*trackMetadata)
	mutex := sync.Mutex{}
	total := tracks.Cardinality()

	err := parallelize(total, tracks.Iter(), func(item interface{}) errors.Error {

		path, ok := item.(string)
		if !ok {
			return errors.Errorf("only put strings in unique track index")
		}

		metadata, err := readTrackMetadata(path)
		if err != nil {
			return err
		}

		mutex.Lock()
		tracksMetadata[path] = metadata
		mutex.Unlock()

		return nil

	})
	if err != nil {
		return tracksMetadata, err
	}

	return tracksMetadata, nil

}

func readTrackMetadata(path string) (*trackMetadata, errors.Error) {

	relativePath, err := filepath.Rel(itunesMusicDir, path)
	if err != nil {
		return nil, errors.Wrap(err, 0)
	}

	cleanedPath, err := cleanPath(relativePath)
	if err != nil {
		return nil, errors.Wrap(err, 0)
	}

	md5, err := computeMd5(path)
	if err != nil {
		return nil, errors.Wrap(err, 0)
	}

	var info infoResult

	isMp3 := strings.HasSuffix(path, ".mp3")
	isMp4 := strings.HasSuffix(path, ".m4a")

	if !isMp3 && !isMp4 {
		return nil, errors.Errorf("Unknown file format!")
	}

	if isMp3 {
		info, err = runMP3Info(path)
		if err != nil {
			return nil, errors.Wrap(err, 0)
		}
	}

	if isMp4 {
		info, err = runMP4Info(path)
		if err != nil {
			return nil, errors.Wrap(err, 0)
		}
	}

	metadata := trackMetadata{
		OriginalPath: path,
		CleanedPath:  cleanedPath,
		Artist:       info.Artist,
		Name:         info.Name,
		Length:       info.Length,
		Hash:         md5,
		Codec:        info.Codec,
	}

	return &metadata, nil

}

func isMn(r rune) bool {
	return unicode.Is(unicode.Mn, r) // Mn: nonspacing marks
}

func cleanPath(path string) (string, error) {

	t := transform.Chain(norm.NFD, transform.RemoveFunc(isMn), norm.NFC)
	path, _, err := transform.String(t, path)
	if err != nil {
		return "", err
	}

	return forbiddenCharsRegexp.ReplaceAllString(path, "_"), nil

}

type infoResult struct {
	Length float64
	Codec  codec
	Artist string
	Name   string
}

func runMP4Info(path string) (infoResult, errors.Error) {

	result := infoResult{}

	cmd := exec.Command("mp4info", path)
	b, err := cmd.Output()
	if err != nil {
		return result, errors.WrapPrefix(err, "Error running mp4info: ", 0)
	}

	output := string(b)
	matches := mp4InfoRegexp.FindStringSubmatch(output)

	codecStr := matches[1]
	lengthStr := matches[2]
	artist := matches[3]
	name := matches[4]

	switch codecStr {
	case "MPEG-4 AAC LC":
		result.Codec = codecAAC
	case "alac":
		result.Codec = codecALAC
	}

	length, err := strconv.ParseFloat(lengthStr, 64)
	if err != nil {
		return result, errors.Wrap(err, 0)
	}
	result.Length = length

	result.Artist = artist
	result.Name = name

	return result, nil

}

func runMP3Info(path string) (infoResult, errors.Error) {

	result := infoResult{}

	cmd := exec.Command("mp3info", "-p", "%a\\t%t\\t%S", path)
	b, err := cmd.Output()
	if err != nil {
		return result, errors.WrapPrefix(err, "Error running mp3info", 0)
	}

	output := string(b)
	matches := strings.Split(output, "\t")

	artist := matches[0]
	name := matches[1]
	lengthStr := matches[2]

	length, err := strconv.ParseFloat(lengthStr, 64)
	if err != nil {
		return result, errors.Wrap(err, 0)
	}
	result.Length = length

	result.Artist = artist
	result.Name = name
	result.Codec = codecMP3

	return result, nil

}

func computeMd5(path string) (string, errors.Error) {

	var result []byte

	file, err := os.Open(path)
	if err != nil {
		return "", errors.Wrap(err, 0)
	}
	defer file.Close()

	hash := md5.New()
	_, err = io.Copy(hash, file)
	if err != nil {
		return "", errors.Wrap(err, 0)
	}

	sum := fmt.Sprintf("%x", hash.Sum(result))
	return sum, nil

}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	if err == nil {
		return true
	}

	return false
}

func ensureFolderExists(path string) errors.Error {
	err := os.MkdirAll(path, 0755)
	if err != nil {
		return errors.Wrap(err, 0)
	}
	return nil
}

func copyTracks(dest string, metadatas map[string]*trackMetadata) errors.Error {

	total := len(metadatas)
	input := make(chan interface{})

	go func() {
		for _, metadata := range metadatas {
			input <- metadata
		}
		close(input)
	}()

	err := parallelize(total, input, func(item interface{}) errors.Error {

		metadata, ok := item.(*trackMetadata)
		if !ok {
			return errors.Errorf("unexpected type")
		}

		err := copyTrack(dest, metadata)
		if err != nil {
			return err
		}

		return nil

	})
	if err != nil {
		return err
	}

	return nil

}

func copyTrack(dest string, metadata *trackMetadata) errors.Error {

	destPath := filepath.Join(dest, metadata.CleanedPath)

	shouldCopy, err := needsCopy(destPath, metadata)
	if err != nil {
		return err
	}

	if shouldCopy {

		err = ensureFolderExists(filepath.Dir(destPath))
		if err != nil {
			return err
		}

		err = copyFile(metadata.OriginalPath, destPath)
		if err != nil {
			return err
		}

		if metadata.Codec != codecALAC {
			err = runAACGain(destPath)
			if err != nil {
				return err
			}
		}

		err = writeHashToXattr(destPath, metadata.Hash)
		if err != nil {
			return err
		}
	}

	return nil

}

func needsCopy(destPath string, srcMetadata *trackMetadata) (bool, errors.Error) {

	_, err := os.Stat(destPath)
	if err != nil { // destPath does not exist
		return true, nil
	}

	md5, err := readHashFromXattr(destPath)
	if err != nil {
		return true, errors.Wrap(err, 0)
	}

	if md5 != srcMetadata.Hash {
		return true, nil
	}

	return false, nil

}

func copyFile(src, dst string) errors.Error {

	in, err := os.Open(src)
	if err != nil {
		return errors.Wrap(err, 0)
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return errors.Wrap(err, 0)
	}
	defer out.Close()

	if _, err = io.Copy(out, in); err != nil {
		return errors.Wrap(err, 0)
	}

	err = out.Sync()
	if err != nil {
		return errors.Wrap(err, 0)
	}

	return nil
}

func readHashFromXattr(path string) (string, errors.Error) {

	out, err := xattr.Getxattr(path, hashXattrName)
	if err != nil {
		return "", errors.Wrap(err, 0)
	}
	return string(out), nil
}

func writeHashToXattr(path string, value string) errors.Error {
	err := xattr.Setxattr(path, hashXattrName, []byte(value))
	if err != nil {
		return errors.Wrap(err, 0)
	}

	return nil
}

func runAACGain(path string) errors.Error {
	cmd := exec.Command("nice", "/usr/local/bin/aacgain", "-r", "-k", "-s", "r", "-d", "9", path)
	err := cmd.Run()
	if err != nil {
		return errors.Wrap(err, 0)
	}
	return nil
}

func exportPlaylists(destPath string, playlists map[string]m3u.Playlist, metadatas map[string]*trackMetadata) errors.Error {

	for name, pl := range playlists {

		newPl := make(m3u.Playlist, len(pl))

		for i, track := range pl {

			metadata := metadatas[track.Path]

			newTrack := m3u.Track{
				Path:  metadata.CleanedPath,
				Time:  int64(metadata.Length),
				Title: fmt.Sprintf("%s - %s", metadata.Artist, metadata.Name),
			}

			newPl[i] = newTrack

		}

		out, err := os.Create(filepath.Join(destPath, name))
		if err != nil {
			return errors.Wrap(err, 0)
		}
		defer out.Close()

		_, err = newPl.WriteTo(out)
		if err != nil {
			return errors.Wrap(err, 0)
		}

		err = out.Sync()
		if err != nil {
			return errors.Wrap(err, 0)
		}

	}

	return nil
}

func parallelize(total int, input <-chan interface{}, callback func(item interface{}) errors.Error) errors.Error {

	bar := pb.StartNew(total).Prefix("                   ")
	sem := make(chan bool, runtime.NumCPU())

	stopped := false
	var lastErr errors.Error

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	go func() {
		for _ = range c {
			log.Println("Received CTRL+C, waiting for current item to finish")
			stopped = true
		}
	}()

	for item := range input {

		if stopped {
			break
		}

		sem <- true
		go func(item interface{}) {
			defer func() { <-sem }()

			err := callback(item)
			if err != nil {
				stopped = true
				lastErr = err
			}

			bar.Increment()
		}(item)

	}

	for i := 0; i < cap(sem); i++ {
		sem <- true
	}

	bar.Finish()
	signal.Reset(os.Interrupt)
	close(c)

	if lastErr != nil {
		return lastErr
	}
	return nil

}
