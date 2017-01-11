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
	// DefaultDirPermissions is the default permissions for directories created by gongflow
	DefaultDirPermissions os.FileMode = 0777
	// DefaultFilePermissions is the default permissions for directories created by gongflow
	DefaultFilePermissions os.FileMode = 0600
	// ErrNoTempDir is returned when the temp directory doesn't exist
	ErrNoTempDir = errors.New("gongflow: the temporary directory doesn't exist")
	// ErrCantCreateDir is returned wwhen the temporary directory doesn't exist
	ErrCantCreateDir = errors.New("gongflow: can't create a directory under the temp directory")
	// ErrCantWriteFile is returned when it can't create a directory under the temp directory
	ErrCantWriteFile = errors.New("gongflow: can't write to a file under the temp directory")
	// ErrCantReadFile is returned when it can't read a file under the temp directory (or got back bad data)
	ErrCantReadFile = errors.New("gongflow: can't read a file under the temp directory (or got back bad data)")
	// ErrCantDelete is return when it can't delete a file/directory under the temp directory
	ErrCantDelete                   = errors.New("gongflow: can't delete a file/directory under the temp directory")
	alreadyCheckedDirectory         = false
	lastCheckedDirectoryError error // = nil
)

// NgFlowData is all the data listed in the "How do I set it up with my server?" section of the ng-flow
// README.md https://github.com/flowjs/flow.js/blob/master/README.md
type NgFlowData struct {
	// ChunkNumber is the index of the chunk in the current upload. First chunk is 1 (no base-0 counting here).
	ChunkNumber int
	// TotalChunks is the total number of chunks.
	TotalChunks int
	// ChunkSize is the general chunk size. Using this value and TotalSize you can calculate the total number of chunks. The "final chunk" can be anything less than 2x chunk size.
	ChunkSize int
	// TotalSize is the total file size.
	TotalSize int
	// TotalSize is a unique identifier for the file contained in the request.
	Identifier string
	// Filename is the original file name (since a bug in Firefox results in the file name not being transmitted in chunk multichunk posts).
	Filename string
	// RelativePath is the file's relative path when selecting a directory (defaults to file name in all browsers except Chrome)
	RelativePath string
}

// ChunkFlowData does exactly what it says on the tin, it extracts all the flow data from a request object and puts
// it into a nice little struct for you
func ChunkFlowData(r *http.Request) (NgFlowData, error) {
	var err error
	ngfd := NgFlowData{}
	ngfd.ChunkNumber, err = strconv.Atoi(r.FormValue("flowChunkNumber"))
	if err != nil {
		return ngfd, errors.New("Bad ChunkNumber")
	}
	ngfd.TotalChunks, err = strconv.Atoi(r.FormValue("flowTotalChunks"))
	if err != nil {
		return ngfd, errors.New("Bad TotalChunks")
	}
	ngfd.ChunkSize, err = strconv.Atoi(r.FormValue("flowChunkSize"))
	if err != nil {
		return ngfd, errors.New("Bad ChunkSize")
	}
	ngfd.TotalSize, err = strconv.Atoi(r.FormValue("flowTotalSize"))
	if err != nil {
		return ngfd, errors.New("Bad TotalSize")
	}
	ngfd.Identifier = r.FormValue("flowIdentifier")
	if ngfd.Identifier == "" {
		return ngfd, errors.New("Bad Identifier")
	}
	ngfd.Filename = r.FormValue("flowFilename")
	if ngfd.Filename == "" {
		return ngfd, errors.New("Bad Filename")
	}
	ngfd.RelativePath = r.FormValue("flowRelativePath")
	if ngfd.RelativePath == "" {
		return ngfd, errors.New("Bad RelativePath")
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
		return "Directory is broken: " + err.Error(), http.StatusInternalServerError
	}
	_, chunkFile := buildPathChunks(tempDir, ngfd)
	ChunkNumberString := strconv.Itoa(ngfd.ChunkNumber)
	dat, err := ioutil.ReadFile(chunkFile)
	if err != nil {
		// every thing except for 200, 201, 202, 404, 415. 500, 501
		return "The chunk " + ngfd.Identifier + ":" + ChunkNumberString + " isn't started yet!", http.StatusNotAcceptable
	}
	// An exception for large last chunks, according to ng-flow the last chunk can be anywhere less
	// than 2x the chunk size unless you haave forceChunkSize on... seems like idiocy to me, but alright.
	if ngfd.ChunkNumber != ngfd.TotalChunks && ngfd.ChunkSize != len(dat) {
		return "The chunk " + ngfd.Identifier + ":" + ChunkNumberString + " is the wrong size!", http.StatusInternalServerError
	}

	return "The chunk " + ngfd.Identifier + ":" + ChunkNumberString + " looks great!", http.StatusOK
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
	filePath := path.Join(tempDir, ngfd.Identifier)
	chunkFile := path.Join(filePath, strconv.Itoa(ngfd.ChunkNumber))
	return filePath, chunkFile
}

// combineChunks will take the chunks uploaded, and combined them into a single file with the
// name as uploaded from the NgFlowData, and it will clean up the chunks as it goes.
func combineChunks(fileDir string, ngfd NgFlowData) (string, error) {
	combinedName := path.Join(fileDir, ngfd.Filename)
	cn, err := os.Create(combinedName)
	if err != nil {
		return "", err
	}

	files, err := ioutil.ReadDir(fileDir)
	if err != nil {
		return "", err
	}
	for _, f := range files {
		fl := path.Join(fileDir, f.Name())
		// make sure, we not copy the same file in the final file.
		// the files array contain the full uploaded file name too.
		if fl != combinedName {
			dat, err := ioutil.ReadFile(fl)
			if err != nil {
				return "", err
			}
			_, err = cn.Write(dat)
			if err != nil {
				return "", err
			}
			err = os.Remove(fl)
			if err != nil {
				return "", err
			}
		}
	}

	err = cn.Close()
	if err != nil {
		return "", err
	}
	return combinedName, nil
}

// allChunksUploaded checks if the file is completely uploaded (based on total size)
func allChunksUploaded(tempDir string, ngfd NgFlowData) bool {
	chunksPath := path.Join(tempDir, ngfd.Identifier)
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
	if totalSize == int64(ngfd.TotalSize) {
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
