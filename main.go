package main

import (
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"unicode"

	"gitlab.kohlby.fr/adrienkohlbecker/ci/utils/errors"

	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"

	"github.com/deckarep/golang-set"
	"github.com/ushis/m3u"
)

var tracks mapset.Set
var forbiddenCharsRegexp = regexp.MustCompile("[^a-zA-Z0-9\\.\\/ ]")

const copyParalellism = 5

var playlistsToExport = []string{"BEST", "GOOGLE PLAY", "DNB", "TRANCE", "ELECTRONNIE", "ABOU", "COLINE", "ELECTROSYLVESTRE", "STARRED", "GRATTE", "PARTY"}

const (
	CodecUnknown = iota
	CodecMP3
	CodecAAC
	CodecALAC
)

func init() {
	tracks = mapset.NewSet()
}

func main() {

	currentUser, err := user.Current()
	if err != nil {
		panic(err)
	}

	basePath := filepath.Join(currentUser.HomeDir, "Music/iTunes/iTunes Media/Music")
	destPath := filepath.Join(currentUser.HomeDir, "exported_playlists")

	err = os.MkdirAll(destPath, 0755)
	if err != nil {
		panic(err)
	}

	m3uPath, err := ioutil.TempDir(os.TempDir(), "m3us")
	if err != nil {
		panic(err)
	}

	defer os.RemoveAll(m3uPath)

	cmd := exec.Command(
		"java",
		"-jar",
		"./vendor/iTunesExportScala-2.2.2/itunesexport.jar",
		"-fileTypes=ALL",
		fmt.Sprintf("-includePlaylist=%s", strings.Join(playlistsToExport, ",")),
		fmt.Sprintf("-outputDir=%s", m3uPath),
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err = cmd.Run()
	if err != nil {
		log.Fatal(err)
	}

	paths := []string{}

	err = filepath.Walk(m3uPath, func(path string, info os.FileInfo, walkErr error) error {

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
		panic(err)
	}

	playlists, err := addPlaylists(paths)
	if err != nil {
		panic(err)
	}

	err = copyTracks(basePath, destPath, tracks)
	if err != nil {
		panic(err)
	}

	err = exportPlaylists(basePath, destPath, playlists)
	if err != nil {
		panic(err)
	}

}

func isMn(r rune) bool {
	return unicode.Is(unicode.Mn, r) // Mn: nonspacing marks
}

func cleanPath(basePath, destPath, path string) (string, error) {

	newPath := strings.Replace(path, basePath, destPath, 1)

	t := transform.Chain(norm.NFD, transform.RemoveFunc(isMn), norm.NFC)
	newPath, _, err := transform.String(t, newPath)
	if err != nil {
		return "", err
	}

	return forbiddenCharsRegexp.ReplaceAllString(newPath, "_"), nil

}

func exportPlaylists(basePath, destPath string, playlists map[string]m3u.Playlist) error {

	for name, pl := range playlists {

		newPl := make(m3u.Playlist, len(pl))

		for i, track := range pl {

			cleanPath, err := cleanPath(basePath, destPath, track.Path)
			if err != nil {
				return err
			}

			relativePath, err := filepath.Rel(destPath, cleanPath)
			if err != nil {
				return err
			}

			newTrack := m3u.Track{
				Path:  relativePath,
				Time:  track.Time,
				Title: track.Title,
			}

			newPl[i] = newTrack

		}

		out, err := os.Create(filepath.Join(destPath, name))
		if err != nil {
			return err
		}
		defer out.Close()

		_, err = newPl.WriteTo(out)
		if err != nil {
			return err
		}

		err = out.Sync()
		if err != nil {
			return err
		}

		fmt.Println(name)

	}

	return nil
}

func digester(done <-chan bool, basePath, destPath string, trackChan <-chan m3u.Track, errChan chan<- error) {
	for track := range trackChan {
		err := copyTrack(basePath, destPath, track)
		select {
		case errChan <- err:
		case <-done:
			return
		}
	}
}

func copyTracks(basePath, destPath string, tracks mapset.Set) error {

	iterator := make(chan m3u.Track)
	go func() {
		for t := range tracks.Iter() {
			iterator <- t.(m3u.Track)
		}
		close(iterator)
	}()

	done := make(chan bool)
	defer close(done)

	errChan := make(chan error)
	var wg sync.WaitGroup

	wg.Add(copyParalellism)
	for i := 0; i < copyParalellism; i++ {
		go func() {
			digester(done, basePath, destPath, iterator, errChan)
			wg.Done()
		}()
	}

	go func() {
		wg.Wait()
		close(errChan)
	}()

	for err := range errChan {
		if err != nil {
			return err
		}
	}

	return nil

}

func copyTrack(basePath, destPath string, track m3u.Track) error {

	fmt.Println(track.Path)

	cleanPath, err := cleanPath(basePath, destPath, track.Path)
	if err != nil {
		return err
	}

	err = os.MkdirAll(filepath.Dir(cleanPath), 0755)
	if err != nil {
		return err
	}

	err = copyFile(track.Path, cleanPath)
	if err != nil {
		return err
	}

	return nil

}

func copyFile(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() {
		cerr := out.Close()
		if err == nil {
			err = cerr
		}
	}()
	if _, err = io.Copy(out, in); err != nil {
		return err
	}
	err = out.Sync()
	if err != nil {
		return err
	}

	codec, err := detectCodec(dst)
	if err != nil {
		return err
	}

	if codec != CodecALAC {

		cmd := exec.Command("nice", "/usr/local/bin/aacgain", "-r", "-k", "-s", "r", "-d", "9", dst)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		err := cmd.Run()
		if err != nil {
			return err
		}

	}

	return
}

func detectCodec(file string) (int, errors.Error) {

	if strings.HasSuffix(file, ".mp3") {
		return CodecMP3, nil
	}

	if strings.HasSuffix(file, ".m4a") {

		cmd := exec.Command("mp4info", file)
		cmd.Stderr = os.Stderr
		out, err := cmd.Output()
		if err != nil {
			return CodecUnknown, errors.Wrap(err, 0)
		}

		sout := string(out)
		if strings.Contains(sout, "audio	alac") {
			return CodecALAC, nil
		}

		if strings.Contains(sout, "audio	MPEG-4 AAC") {
			return CodecAAC, nil
		}

		return CodecUnknown, errors.Errorf("Format unrecognized by mp4info: %s", file)

	}

	return CodecUnknown, errors.Errorf("Unknow file format: %s", file)

}

func addPlaylists(paths []string) (map[string]m3u.Playlist, error) {

	playlists := make(map[string]m3u.Playlist)
	var mutex sync.Mutex

	wg := sync.WaitGroup{}
	wg.Add(len(paths))

	errChan := make(chan error)

	for _, path := range paths {

		go func(path string) {

			pl, err := addPlaylist(path)
			if err != nil {
				errChan <- err
				return
			}
			name := filepath.Base(path)

			mutex.Lock()
			playlists[name] = pl
			mutex.Unlock()

			errChan <- nil
			wg.Done()

		}(path)

	}

	go func() {
		wg.Wait()
		close(errChan)
	}()

	for err := range errChan {
		if err != nil {
			return nil, err
		}
	}

	return playlists, nil
}

func addPlaylist(path string) (m3u.Playlist, error) {

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	pl, err := m3u.Parse(f)
	if err != nil {
		return nil, err
	}

	for i, track := range pl {
		track.Path = strings.Replace(track.Path, "file://", "", 1)
		tracks.Add(track)
		pl[i] = track
	}

	return pl, nil

}
