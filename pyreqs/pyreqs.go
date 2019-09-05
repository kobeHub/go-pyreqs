package pyreqs

import (
	"bufio"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/scylladb/go-set"
	"github.com/scylladb/go-set/strset"
	"github.com/tidwall/gjson"
	"gopkg.in/src-d/go-git.v4"
	trans_http "gopkg.in/src-d/go-git.v4/plumbing/transport/http"
)

const (
	LOCAL uint = iota
	REMOTE
)

type PyReqsError struct {
	ops string
	msg string
}

func (p PyReqsError) Error() string {
	return fmt.Sprintf("pyreqs error: %s -- %s", p.ops, p.msg)
}

func newError(ops string, msg string) PyReqsError {

	return PyReqsError{ops, msg}
}

func readFile(file_name string) (string, error) {
	fd, err := os.Open(file_name)
	if err != nil {
		return "", err
	}
	defer fd.Close()
	reader := bufio.NewReader(fd)
	buf := make([]byte, 1024)
	chunks := make([]byte, 0)
	for {
		written, err := reader.Read(buf)
		if err != nil && err != io.EOF {
			return "", newError("read file "+file_name, err.Error())
		}
		if written == 0 {
			break
		}
		chunks = append(chunks, buf[:written]...)
	}
	return string(chunks), nil
}

func CloneRepo(url string, accessToken string) (string, error) {
	// tempdir store the repo
	dir, err := ioutil.TempDir("", "remote_repos")
	if err != nil {
		return "", newError("make dir", err.Error())
	}

	_, err = git.PlainClone(dir, false, &git.CloneOptions{
		URL: url,
		Auth: &trans_http.BasicAuth{
			Username: "a",
			Password: accessToken,
		},
	})
	if err != nil {
		return "", newError("clone repo", err.Error())
	}
	return dir, nil
}

func GetAllImports(root string, extraIgnoreDirs ...string) ([]string, error) {
	importList := make([]string, 0)
	candidates := make([]string, 0)
	regex1 := regexp.MustCompile(`^(\s+)?import (.+)$`)
	regex2 := regexp.MustCompile(`^(\s+)?from (.*?) import (?:.*)$`)
	regex3 := regexp.MustCompile(`^\S*`)

	ignoreDirs := []string{".hg", ".git", ".mypy_cache", ".tox", "__pycache__", "env", "venv"}
	if len(extraIgnoreDirs) != 0 {
		for _, e := range extraIgnoreDirs {
			ignoreDirs = append(ignoreDirs, filepath.Base(e))
		}
	}
	sort.Strings(ignoreDirs)
	ignoreNum := len(ignoreDirs)

	fileList := make([]string, 0, 10)
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if info.IsDir() {
			name := strings.ToLower(info.Name())
			if i := sort.SearchStrings(ignoreDirs, name); i < ignoreNum && ignoreDirs[i] == name {
				return filepath.SkipDir
			}
			candidates = append(candidates, info.Name())
		} else {
			name := info.Name()
			if filepath.Ext(name) == ".py" {
				fileList = append(fileList, path)
				candidates = append(candidates, name[:len(name)-3])
			}
		}
		return nil
	})
	if err != nil {
		return importList, newError("walk directory", err.Error())
	}

	for _, file := range fileList {
		if filepath.Base(file) == "__init__.py" {
			continue
		}

		content, err := readFile(file)
		if err != nil {
			return importList, err
		}

		for _, line := range strings.Split(content, "\n") {
			for _, item := range regex1.FindAllString(line, -1) {
				var library string
				item = strings.TrimSpace(item)
				if strings.Contains(item, ",") {
					item = strings.TrimSpace(item[6:])
					for _, word := range strings.Split(item, ",") {
						library = regex3.FindString(strings.TrimSpace(word))

						if strings.Contains(library, ".") {
							library = strings.Split(library, ".")[0]
						}
						importList = append(importList, library)
					}
				} else {
					library = strings.TrimSpace(strings.Split(item, " ")[1])

					if strings.Contains(library, ".") {
						library = strings.Split(library, ".")[0]
					}
					importList = append(importList, library)
				}
			}
			for _, item := range regex2.FindAllString(line, -1) {
				item = strings.TrimSpace(item)
				library := strings.TrimSpace(strings.Split(item, " ")[1])
				if strings.Contains(library, ".") {
					if library == "." || library == ".." {
						continue
					}
					library = strings.Split(library, ".")[0]
				}
				importList = append(importList, library)
			}
		}
	}
	stdlibCon, _ := readFile("pyreqs/stdlib")

	originSet := set.NewStringSet(importList...)
	extraSet := set.NewStringSet(candidates...)
	stdlibSet := set.NewStringSet(strings.Split(stdlibCon, "\n")...)
	importList = strset.Difference(strset.Difference(originSet, extraSet), stdlibSet).List()

	return getPkgName(importList), nil
}

func getPkgName(importList []string) (pkgs []string) {
	mappingCon, _ := readFile("pyreqs/mapping")
	dict := make(map[string]string)
	pkgs = make([]string, 0, len(importList))
	for _, line := range strings.Split(mappingCon, "\n") {
		line = strings.TrimSpace(line)
		cons := strings.Split(line, ":")
		if len(cons) == 2 {
			dict[cons[0]] = cons[1]
		}
	}

	for _, item := range importList {
		if item == "tensorflow" {
			pkgs = append(pkgs, "tensorflow-gpu")
		}
		if i, ok := dict[item]; ok {
			pkgs = append(pkgs, i)
		} else {
			pkgs = append(pkgs, item)
		}
	}
	return
}

func GetRequirementsLocal(importList []string, pypiServer string) []string {
	requirements := make([]string, 0)
	resultCh := make(chan string, 10)
	timeOut := time.Second * 2
	client := &http.Client{Timeout: timeOut}
	var badNum int32 = 0
	for _, one := range importList {
		//log.Println(item)
		go func(item string) {
			request, _ := http.NewRequest("GET", fmt.Sprintf("%s/%s/json", pypiServer, item), nil)
			resp, _ := client.Do(request)
			defer resp.Body.Close()

			if resp.StatusCode == 200 {
				con, _ := ioutil.ReadAll(resp.Body)
				jsonCon := string(con)
				release := gjson.Get(jsonCon, "info.version")
				resultCh <- item + "==" + release.String()
			} else if resp.StatusCode == 404 {
				atomic.AddInt32(&badNum, int32(1))
				log.Println("PyReq:", "The package does not exist:", item)
			} else {
				atomic.AddInt32(&badNum, int32(1))
				log.Println("PyReq:", "Pypi server error:", pypiServer)
			}
			return
		}(one)
	}

	for {
		select {
		case res := <-resultCh:
			requirements = append(requirements, res)
		default:
			if len(requirements) == len(importList)-int(badNum) {
				goto DONE
			}
		}
	}
DONE:
	return requirements
}

func GetRequirementsRemote(url string, accessToken string, ignoreDirs ...string) []string {
	dirName, err := CloneRepo(url, accessToken)
	if err != nil {
		panic(err)
	}
	imports, err := GetAllImports(dirName, ignoreDirs...)
	os.RemoveAll(dirName)
	return GetRequirementsLocal(imports, "https://pypi.org/pypi")
}

func ToFile(requires []string, filePath string) {
	fd, err := os.Create(filePath)
	if err != nil {
		panic(err)
	}
	defer fd.Close()

	for _, item := range requires {
		_, err := fd.WriteString(item + "\n")
		if err != nil {
			panic(err)
		}
	}
}
