package drive

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"
	"strconv"

	"google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
)

type DownloadArgs struct {
	Out       io.Writer
	Progress  io.Writer
	Id        string
	Path      string
	Force     bool
	Skip      bool
	Recursive bool
	Delete    bool
	Stdout    bool
	Timeout   time.Duration
}

func (self *Drive) Download(args DownloadArgs) error {
	if args.Recursive {
		return self.downloadRecursive(args)
	}

	f, err := self.service.Files.Get(args.Id).Fields("id", "name", "size", "mimeType", "md5Checksum").Do()
	if err != nil {
		return fmt.Errorf("Failed to get file: %s", err)
	}

	if isDir(f) {
		return fmt.Errorf("'%s' is a directory, use --recursive to download directories", f.Name)
	}

	if !isBinary(f) {
		return fmt.Errorf("'%s' is a google document and must be exported, see the export command", f.Name)
	}

	bytes, rate, err := self.downloadBinary(f, args)
	if err != nil {
		return err
	}

	if !args.Stdout {
		fmt.Fprintf(args.Out, "Downloaded %s at %s/s, total %s\n", f.Id, formatSize(rate, false), formatSize(bytes, false))
	}

	if args.Delete {
		err = self.deleteFile(args.Id)
		if err != nil {
			return fmt.Errorf("Failed to delete file: %s", err)
		}

		if !args.Stdout {
			fmt.Fprintf(args.Out, "Removed %s\n", args.Id)
		}
	}
	return err
}

type DownloadQueryArgs struct {
	Out       io.Writer
	Progress  io.Writer
	Query     string
	Path      string
	Force     bool
	Skip      bool
	Recursive bool
}

func (self *Drive) DownloadQuery(args DownloadQueryArgs) error {
	listArgs := listAllFilesArgs{
		query:  args.Query,
		fields: []googleapi.Field{"nextPageToken", "files(id,name,mimeType,size,md5Checksum)"},
	}
	files, err := self.listAllFiles(listArgs)
	if err != nil {
		return fmt.Errorf("Failed to list files: %s", err)
	}

	downloadArgs := DownloadArgs{
		Out:      args.Out,
		Progress: args.Progress,
		Path:     args.Path,
		Force:    args.Force,
		Skip:     args.Skip,
	}

	for _, f := range files {
		if isDir(f) && args.Recursive {
			err = self.downloadDirectory(f, downloadArgs)
		} else if isBinary(f) {
			_, _, err = self.downloadBinary(f, downloadArgs)
		}

		if err != nil {
			return err
		}
	}

	return nil
}

func (self *Drive) downloadRecursive(args DownloadArgs) error {
	f, err := self.service.Files.Get(args.Id).Fields("id", "name", "size", "mimeType", "md5Checksum").Do()
	if err != nil {
		return fmt.Errorf("Failed to get file: %s", err)
	}

	if isDir(f) {
		return self.downloadDirectory(f, args)
	} else if isBinary(f) {
		_, _, err = self.downloadBinary(f, args)
		return err
	}

	return nil
}

func (self *Drive) downloadBinary(f *drive.File, args DownloadArgs) (int64, int64, error) {
	// Get timeout reader wrapper and context
	timeoutReaderWrapper, ctx := getTimeoutReaderWrapperContext(args.Timeout)

	res, err := self.service.Files.Get(f.Id).Context(ctx).Download()
	if err != nil {
		if isTimeoutError(err) {
			return 0, 0, fmt.Errorf("Failed to download file: timeout, no data was transferred for %v", args.Timeout)
		}
		return 0, 0, fmt.Errorf("Failed to download file: %s", err)
	}

	// Close body on function exit
	defer res.Body.Close()

	return self.saveFile(saveFileArgs{
		out:           args.Out,
		body:          timeoutReaderWrapper(res.Body),
		contentLength: res.ContentLength,
		fpath:         args.Path,
		fname:         f.Name,
		force:         args.Force,
		skip:          args.Skip,
		stdout:        args.Stdout,
		progress:      args.Progress,
	})
}

type saveFileArgs struct {
	out           io.Writer
	body          io.Reader
	contentLength int64
	fpath         string
	fname         string
	force         bool
	skip          bool
	stdout        bool
	progress      io.Writer
}

func (self *Drive) saveFile(args saveFileArgs) (int64, int64, error) {
	// Wrap response body in progress reader
	srcReader := getProgressReader(args.body, args.progress, args.contentLength)

	// Lookup API for file metadata
	sf, err := getSerenityFile(args.fname, args.contentLength)
	isSerenityFile := (err == nil)
	var fpath string

	if (isSerenityFile && sf.Scrambled) {
		fpath = filepath.Join(args.fpath, sf.Name)
		handleSerenitySource(srcReader)
	} else {
		fpath = filepath.Join(args.fpath, args.fname)
	}

	if !args.stdout {
		fmt.Fprintf(args.out, "Downloading %s -> %s\n", args.fname, fpath)
	}

	if args.stdout {
		// Write file content to stdout
		_, err := io.Copy(args.out, srcReader)
		return 0, 0, err
	}

	// Check if file exists to force
	if !args.skip && !args.force && fileExists(fpath) {
		return 0, 0, fmt.Errorf("File '%s' already exists, use --force to overwrite or --skip to skip", fpath)
	}

	//Check if file exists to skip
	if args.skip && fileExists(fpath) {
		fmt.Printf("File '%s' already exists, skipping\n", fpath)
		return 0, 0, nil
	}

	// Ensure any parent directories exists
	if err := mkdir(fpath); err != nil {
		return 0, 0, err
	}

	// Download to tmp file
	tmpPath := fpath + ".incomplete"
	if isSerenityFile && fileExists(tmpPath) {
		bytes, rate, err := resumeSaveFile(args, tmpPath, sf)
		if err == nil {
			os.Rename(tmpPath, fpath)
			return bytes, rate, err
		}
	}

	// Create new file
	outFile, err := os.Create(tmpPath)
	if err != nil {
		return 0, 0, fmt.Errorf("Unable to create new file: %s", err)
	}

	started := time.Now()

	// Save file to disk
	bytes, err := io.Copy(outFile, srcReader)
	if err != nil {
		outFile.Close()

		if !isSerenityFile {
			os.Remove(tmpPath)
		}

		return 0, 0, fmt.Errorf("Failed saving file: %s", err)
	}

	// Calculate average download rate
	rate := calcRate(bytes, started, time.Now())

	// Close File
	outFile.Close()

	// Rename tmp file to proper filename
	return bytes, rate, os.Rename(tmpPath, fpath)
}

func (self *Drive) downloadDirectory(parent *drive.File, args DownloadArgs) error {
	listArgs := listAllFilesArgs{
		query:  fmt.Sprintf("'%s' in parents", parent.Id),
		fields: []googleapi.Field{"nextPageToken", "files(id,name)"},
	}
	files, err := self.listAllFiles(listArgs)
	if err != nil {
		return fmt.Errorf("Failed listing files: %s", err)
	}

	newPath := filepath.Join(args.Path, parent.Name)

	for _, f := range files {
		// Copy args and update changed fields
		newArgs := args
		newArgs.Path = newPath
		newArgs.Id = f.Id
		newArgs.Stdout = false

		err = self.downloadRecursive(newArgs)
		if err != nil {
			return err
		}
	}

	return nil
}

func isDir(f *drive.File) bool {
	return f.MimeType == DirectoryMimeType
}

func isBinary(f *drive.File) bool {
	return f.Md5Checksum != ""
}

func resumeSaveFile(args saveFileArgs, tmpPath string, sf Fi) (int64, int64, error) {
	outFile, err := os.OpenFile(tmpPath, os.O_APPEND|os.O_WRONLY, 0666)
	if err != nil {
		return 0, 0, fmt.Errorf("Unable to append to file: %s", err)
	}

	fmt.Printf("Attempting resume on file '%s'\n", tmpPath)
	if fsize, err := fileSize(tmpPath); err == nil && fsize > 0 {
		started := time.Now()
		bytesToSkip := SerenityHeaderLen + fsize
		bytes := sf.FileSize - bytesToSkip
		if bytes == 0 {
			return 0, 0, nil
		}

		if bytes > 0 {
			skipFlag := "--start-pos=" + strconv.FormatInt(bytesToSkip, 10)
			fmt.Printf("Skipping %d bytes in remote file\n", bytesToSkip)
			cmd := exec.Command("wget", skipFlag, "-O", "-", sf.Url)
			cmd.Stderr = args.out
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				return 0, 0, err
			}

			err = cmd.Start()
			if err != nil {
				return 0, 0, err
			}

			defer cmd.Wait()

			// Append STDOUT to existing .incomplete file
			bytes, err = io.Copy(outFile, stdout)

			// Calculate average download rate
			rate := calcRate(bytes, started, time.Now())

			// Close File
			outFile.Close()

			// Rename tmp file to proper filename
			return bytes, rate, nil
		}
	}

	// Close File
	outFile.Close()

	// Delete File
	os.Remove(tmpPath)
	return 0, 0, fmt.Errorf("Failed to resume download")
}
