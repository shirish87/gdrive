package drive

import (
	"fmt"
	"os"
	"io"
	"io/ioutil"
	"net/http"
	"encoding/json"
	"strings"
	"strconv"
)

var SerenityURL = os.Getenv("SERENITY_URL")
var SerenityHeaderLen, _ = strconv.ParseInt(os.Getenv("SERENITY_HEADER_LEN"), 10, 64)

type Fi struct {
	Id          string
	Name        string
	Title       string
	FileSize    int64
	Scrambled   bool
}

func makeJsonRequest(url string, target interface{}) error {
	r, err := http.Get(url)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(target)
}

func SerenityFilter(vs []Fi, f func(Fi) bool) []Fi {
	vsf := make([]Fi, 0)
	for _, v := range vs {
		if f(v) {
			vsf = append(vsf, v)
		}
	}
	return vsf
}

func getSerenityFiles(filter string) ([]Fi, error) {
	files := make([]Fi, 0)
	var err error
	if len(SerenityURL) > 0 {
		err = makeJsonRequest(fmt.Sprintf(SerenityURL, filter), &files)
	}

	return files, err
}

func getSerenityFile(input string, size int64) (Fi, error) {
	filter := strings.ToLower(input)
	files, err := getSerenityFiles(filter)
	if err != nil {
		return Fi{}, err
	}

	if len(input) > 0 {
		files = SerenityFilter(files, func (f Fi) bool {
			if (!strings.Contains(strings.ToLower(f.Title), filter) ||
				(size > 0 && size != f.FileSize)) {
				return false
			}

			return true
		})
	}

	if len(files) > 0 {
		return files[0], nil
	}

	return Fi{}, fmt.Errorf("Failed to find %v", input)
}

func handleSerenitySource(srcReader io.Reader) {
	if SerenityHeaderLen > 0 {
		// Discard SerenityHeaderLen-byte header
		io.CopyN(ioutil.Discard, srcReader, SerenityHeaderLen)
	}
}
