// Package gongflow provides server support for ng-flow (https://github.com/flowjs/ng-flow).  If you want a way to instantly
// test it out, grab the gongflow-demo package: http://godoc.org/github.com/patdek/gongflow-demo
package gongflow

import (
	"errors"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path"
	"strconv"
	"time"
)

var (
	DefaultDirPermissions     os.FileMode = 0777
	DefaultFilePermissions    os.FileMode = 0777
	ErrNoTempDir                          = errors.New("gongflow: the temporary directory doesn't exist")
	ErrCantCreateDir                      = errors.New("gongflow: can't create a directory under the temp directory")
	ErrCantWriteFile                      = errors.New("gongflow: can't write to a file under the temp directory")
	ErrCantReadFile                       = errors.New("gongflow: can't read a file under the temp directory (or got back bad data)")
	ErrCantDelete                         = errors.New("gongflow: can't delete a file/directory under the temp directory")
	alreadyCheckedDirectory               = false
	lastCheckedDirectoryError error       = nil
)

// NgFlowData is all the data listed in the "How do I set it up with my server?" section of the ng-flow
// README.md https://github.com/flowjs/flow.js/blob/master/README.md
type NgFlowData struct {
	flowChunkNumber  int    // The index of the chunk in the current upload. First chunk is 1 (no base-0 counting here).
	flowTotalChunks  int    // The total number of chunks.
	flowChunkSize    int    // The general chunk size. Using this value and flowTotalSize you can calculate the total number of chunks. The "final chunk" can be anything less than 2x chunk size.
	flowTotalSize    int    // The total file size.
	flowIdentifier   string // A unique identifier for the file contained in the request.
	flowFilename     string // The original file name (since a bug in Firefox results in the file name not being transmitted in chunk multichunk posts).
	flowRelativePath string // The file's relative path when selecting a directory (defaults to file name in all browsers except Chrome)
}

// ChunkFlowData does exactly what it says on the tin, it extracts all the flow data from a request object and puts
// it into a nice little struct for you
func ChunkFlowData(r *http.Request) (NgFlowData, error) {
	var err error
	ngfd := NgFlowData{}
	ngfd.flowChunkNumber, err = strconv.Atoi(r.FormValue("flowChunkNumber"))
	if err != nil {
		return ngfd, errors.New("Bad flowChunkNumber")
	}
	ngfd.flowTotalChunks, err = strconv.Atoi(r.FormValue("flowTotalChunks"))
	if err != nil {
		return ngfd, errors.New("Bad flowTotalChunks")
	}
	ngfd.flowChunkSize, err = strconv.Atoi(r.FormValue("flowChunkSize"))
	if err != nil {
		return ngfd, errors.New("Bad flowChunkSize")
	}
	ngfd.flowTotalSize, err = strconv.Atoi(r.FormValue("flowTotalSize"))
	if err != nil {
		return ngfd, errors.New("Bad flowTotalSize")
	}
	ngfd.flowIdentifier = r.FormValue("flowIdentifier")
	if ngfd.flowIdentifier == "" {
		return ngfd, errors.New("Bad flowIdentifier")
	}
	ngfd.flowFilename = r.FormValue("flowFilename")
	if ngfd.flowFilename == "" {
		return ngfd, errors.New("Bad flowFilename")
	}
	ngfd.flowRelativePath = r.FormValue("flowRelativePath")
	if ngfd.flowRelativePath == "" {
		return ngfd, errors.New("Bad flowRelativePath")
	}
	return ngfd, nil
}

// ChunkUpload is used to handle a POST from ng-flow, it will return an empty string for chunk upload (incomplete) and when
// all the chunks have been uploaded, it will return the path to the reconstituted file.  So, you can just keep calling it
// until you get back the path to a file.
func ChunkUpload(tempDir string, ngfd NgFlowData, r *http.Request) (string, error) {
	err := checkDirectory(tempDir)
	if err != nil {
		return "", err
	}
	fileDir, chunkFile := buildPathChunks(tempDir, ngfd)
	err = storeChunk(fileDir, chunkFile, ngfd, r)
	if err != nil {
		return "", errors.New("Unable to store chunk" + err.Error())
	}
	if allChunksUploaded(tempDir, ngfd) {
		file, err := combineChunks(fileDir, ngfd)
		if err != nil {
			return "", err
		}
		return file, nil
	}
	return "", nil
}

// ChunkStatus is used to handle a GET from ng-flow, it will return a (message, 200) for when it already has a chunk, and it
// will return a (message, 404 | 500) when a chunk is incomplete or not started.
func ChunkStatus(tempDir string, ngfd NgFlowData) (string, int) {
	err := checkDirectory(tempDir)
	if err != nil {
		return "Directory is broken: " + err.Error(), 500
	}
	_, chunkFile := buildPathChunks(tempDir, ngfd)
	flowChunkNumberString := strconv.Itoa(ngfd.flowChunkNumber)
	dat, err := ioutil.ReadFile(chunkFile)
	if err != nil {
		return "The chunk " + ngfd.flowIdentifier + ":" + flowChunkNumberString + " isn't started yet!", 404
	}
	// An exception for large last chunks, according to ng-flow the last chunk can be anywhere less
	// than 2x the chunk size unless you haave forceChunkSize on... seems like idiocy to me, but alright.
	if ngfd.flowChunkNumber != ngfd.flowTotalChunks && ngfd.flowChunkSize != len(dat) {
		return "The chunk " + ngfd.flowIdentifier + ":" + flowChunkNumberString + " is the wrong size!", 500
	}

	return "The chunk " + ngfd.flowIdentifier + ":" + flowChunkNumberString + " looks great!", 200
}

// ChunksCleanup is used to go through the tempDir and remove any chunks and directories older than
// than the timeoutDur, best to set this VERY conservatively.
func ChunksCleanup(tempDir string, timeoutDur time.Duration) error {
	files, err := ioutil.ReadDir(tempDir)
	if err != nil {
		return err
	}
	for _, f := range files {
		fl := path.Join(tempDir, f.Name())
		finfo, err := os.Stat(fl)
		if err != nil {
			return err
		}

		log.Println(f.Name())
		log.Println(time.Now().Sub(finfo.ModTime()))
		if time.Now().Sub(finfo.ModTime()) > timeoutDur {
			err = os.RemoveAll(fl)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// buildPathChunks simply builds the paths to the ID of the upload, and to the specific Chunk
func buildPathChunks(tempDir string, ngfd NgFlowData) (string, string) {
	filePath := path.Join(tempDir, ngfd.flowIdentifier)
	chunkFile := path.Join(filePath, strconv.Itoa(ngfd.flowChunkNumber))
	return filePath, chunkFile
}

// combineChunks will take the chunks uploaded, and combined them into a single file with the
// name as uploaded from the NgFlowData, and it will clean up the chunks as it goes.
func combineChunks(fileDir string, ngfd NgFlowData) (string, error) {
	combinedName := path.Join(fileDir, ngfd.flowFilename)
	cn, err := os.Create(combinedName)
	if err != nil {
		return "", err
	}
	defer cn.Close()

	files, err := ioutil.ReadDir(fileDir)
	if err != nil {
		return "", err
	}
	for _, f := range files {
		fl := path.Join(fileDir, f.Name())
		dat, err := ioutil.ReadFile(fl)
		if err != nil {
			return "", err
		}
		_, err = cn.Write(dat)
		if err != nil {
			return "", err
		}
		if fl != combinedName { // we don't want to delete the file we just created
			err = os.Remove(fl)
			if err != nil {
				return "", err
			}
		}
	}
	return combinedName, nil
}

// allChunksUploaded checks if the file is completely uploaded (based on total size)
func allChunksUploaded(tempDir string, ngfd NgFlowData) bool {
	chunksPath := path.Join(tempDir, ngfd.flowIdentifier)
	files, err := ioutil.ReadDir(chunksPath)
	if err != nil {
		log.Println(err)
	}
	totalSize := int64(0)
	for _, f := range files {
		fi, err := os.Stat(path.Join(chunksPath, f.Name()))
		if err != nil {
			log.Println(err)
		}
		totalSize += fi.Size()
	}
	if totalSize == int64(ngfd.flowTotalSize) {
		return true
	}
	return false
}

// storeChunk puts the chunk in the request into the right place on disk
func storeChunk(tempDir string, tempFile string, ngfd NgFlowData, r *http.Request) error {
	err := os.MkdirAll(tempDir, DefaultDirPermissions)
	if err != nil {
		return errors.New("Bad directory")
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		return errors.New("Can't access file field")
	}
	data, err := ioutil.ReadAll(file)
	if err != nil {
		return errors.New("Can't read file")
	}
	err = ioutil.WriteFile(tempFile, data, DefaultDirPermissions)
	if err != nil {
		return errors.New("Can't write file")
	}
	return nil
}

// checkDirectory makes sure that we have all the needed permissions to the temp directory to
// read/write/delete.  Expensive operation, so it only does it once.
func checkDirectory(d string) error {
	if alreadyCheckedDirectory {
		return lastCheckedDirectoryError
	}

	alreadyCheckedDirectory = true

	if !directoryExists(d) {
		lastCheckedDirectoryError = ErrNoTempDir
		return lastCheckedDirectoryError
	}

	testName := "5d58061677944334bb616ba19cec5cc4"
	testChunk := "42"
	contentName := "foobie"
	testContent := `For instance, on the planet Earth, man had always assumed that he was more intelligent than 
	dolphins because he had achieved so much—the wheel, New York, wars and so on—whilst all the dolphins had 
	ever done was muck about in the water having a good time. But conversely, the dolphins had always believed 
	that they were far more intelligent than man—for precisely the same reasons.`

	p := path.Join(d, testName, testChunk)
	err := os.MkdirAll(p, DefaultDirPermissions)
	if err != nil {
		lastCheckedDirectoryError = ErrCantCreateDir
		return lastCheckedDirectoryError
	}

	f := path.Join(p, contentName)
	err = ioutil.WriteFile(f, []byte(testContent), DefaultFilePermissions)
	if err != nil {
		lastCheckedDirectoryError = ErrCantWriteFile
		return lastCheckedDirectoryError
	}

	b, err := ioutil.ReadFile(f)
	if err != nil {
		lastCheckedDirectoryError = ErrCantReadFile
		return lastCheckedDirectoryError
	}
	if string(b) != testContent {
		lastCheckedDirectoryError = ErrCantReadFile // TODO: This should probably be a different error
		return lastCheckedDirectoryError
	}

	err = os.RemoveAll(path.Join(d, testName))
	if err != nil {
		lastCheckedDirectoryError = ErrCantDelete
		return lastCheckedDirectoryError
	}

	if os.TempDir() == d {
		log.Println("You should really have a directory just for upload temp (different from system temp).  It is OK, but consider making a subdirectory for it.")
	}

	return nil
}

// directoryExists checks if the directory exists of course!
func directoryExists(d string) bool {
	finfo, err := os.Stat(d)
	if err == nil && finfo.IsDir() {
		return true
	}
	return false
}
