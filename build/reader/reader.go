package reader

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/appministry/firebuild/build/commands"
	bcErrors "github.com/appministry/firebuild/build/errors"
	"github.com/moby/buildkit/frontend/dockerfile/parser"

	git "github.com/go-git/go-git/v5"
)

// ReadFromString reads commands from string.
//
// - literal Dockerfile content, ADD and COPY will not work
// - http:// or httpL// URL
// - git+http:// and git+https:// URL
//   the format is: git+http(s)://host:port/path/to/repo.git:/path/to/Dockerfile[#<commit-hash | branch-name | tag-name>]
// - absolute path to the local file
func ReadFromString(input string, tempDirectory string) ([]interface{}, error) {

	if strings.HasPrefix(input, "git+http://") || strings.HasPrefix(input, "git+https://") {
		u, _ := url.Parse(input)

		branchToCheckout := u.Fragment
		pathParts := strings.Split(u.Path, ":")
		if len(pathParts) != 2 {
			return nil, fmt.Errorf("invalid path: %s, expected /org/repo.git:/file/in/repo", u.Path)
		}

		pathInRepo := pathParts[1]
		u.Path = pathParts[0]
		u.Fragment = ""

		repoURL := strings.Replace(strings.Replace(u.String(), "git+http://", "http://", 1), "git+https://", "https://", 1)

		repoDestDir := filepath.Join(tempDirectory, "sources")
		repo, err := git.PlainClone(repoDestDir, false, &git.CloneOptions{
			URL:      repoURL,
			Progress: os.Stdout,
		})
		if err != nil {
			return nil, fmt.Errorf("failed clone: %+v", err)
		}

		if branchToCheckout != "" {
			remotes, err := repo.Remotes()
			if err != nil {
				return nil, fmt.Errorf("failed listing remotes: %+v", err)
			}
			refs, err := remotes[0].List(&git.ListOptions{})
			if err != nil {
				return nil, fmt.Errorf("failed listing remotes: %+v", err)
			}
			for _, ref := range refs {
				if ref.Name().IsBranch() || ref.Name().IsTag() {
					if ref.Hash().String() == branchToCheckout || strings.HasSuffix(ref.Name().String(), fmt.Sprintf("/%s", branchToCheckout)) {
						worktree, err := repo.Worktree()
						if err != nil {
							return nil, fmt.Errorf("failed fetching worktree: %+v", err)
						}
						if err := worktree.Checkout(&git.CheckoutOptions{
							Hash: ref.Hash(),
						}); err != nil {
							return nil, fmt.Errorf("failed checkout %s: %+v", branchToCheckout, err)
						}
						break
					}
				}
			}
		}

		// the Dockerfile is basically:
		filePath := filepath.Join(repoDestDir, pathInRepo)
		statResult, statErr := os.Stat(filePath)
		if statErr != nil {
			return nil, statErr
		}
		if statResult.IsDir() {
			return nil, &bcErrors.ErrorIsDirectory{Input: filePath}
		}
		bytes, err := ioutil.ReadFile(filePath)
		if err != nil && err != io.EOF {
			return nil, err
		}

		return ReadFromBytesWithOriginalSource(bytes, filePath)
	}

	if strings.HasPrefix(input, "http://") || strings.HasPrefix(input, "https://") {
		httpResponse, err := http.Get(input)
		if err != nil {
			return nil, err
		}
		defer httpResponse.Body.Close()
		bytes, err := ioutil.ReadAll(httpResponse.Body)
		if err != nil && err != io.EOF {
			return nil, err
		}
		return ReadFromBytesWithOriginalSource(bytes, input)
	}

	statResult, statErr := os.Stat(input)
	if statErr != nil {

		if statErr == os.ErrNotExist {
			// assume literal input:
			return ReadFromBytes([]byte(input))
		}

		if statErr != os.ErrNotExist {
			return nil, statErr
		}
	}

	if statResult.IsDir() {
		return nil, &bcErrors.ErrorIsDirectory{Input: input}
	}

	bytes, err := ioutil.ReadFile(input)
	if err != nil && err != io.EOF {
		return nil, err
	}
	return ReadFromBytesWithOriginalSource(bytes, input)

}

// ReadFromBytes reads commands from bytes.
// The bytes most often will be the Dockerfile string literal converted to bytes.
func ReadFromBytes(input []byte) ([]interface{}, error) {
	parserResult, err := parser.Parse(bytes.NewReader(input))
	if err != nil {
		return nil, err
	}
	return ReadFromParserResult(parserResult, "")
}

// ReadFromBytesWithOriginalSource reads commands from bytes and passes
// the original source to the build context.
// Use this method to automatically resolve the ADD / COPY dependencies.
func ReadFromBytesWithOriginalSource(input []byte, originalSource string) ([]interface{}, error) {
	parserResult, err := parser.Parse(bytes.NewReader(input))
	if err != nil {
		return nil, err
	}
	return ReadFromParserResult(parserResult, originalSource)
}

// ReadFromParserResult reads commands from the Dockerfile parser result.
func ReadFromParserResult(parserResult *parser.Result, originalSource string) ([]interface{}, error) {
	output := []interface{}{}
	for _, child := range parserResult.AST.Children {
		switch child.Value {
		case "add":
			values := []string{}
			current := child.Next
			for {
				if current == nil {
					break
				}
				values = append(values, current.Value)
				current = current.Next
			}
			if len(values) == 2 {
				flags := readFlags(child.Flags)
				add := commands.Add{
					OriginalCommand: child.Original,
					OriginalSource:  originalSource,
					Source:          values[0],
					Target:          values[1],
				}
				if chownVal, ok := flags.get("--chown"); ok {
					add.UserFromLocalChown = &commands.User{Value: chownVal}
				}
				output = append(output, add)
				continue
			}
			return output, fmt.Errorf("invalid ADD %q: %d", strings.Join(values, " "), child.StartLine)
		case "arg":
			current := child.Next
			for {
				if current == nil {
					break
				}
				arg, argErr := commands.NewRawArg(current.Value)
				if argErr != nil {
					return output, fmt.Errorf("arg at %d: %+v", child.StartLine, argErr)
				}
				arg.OriginalCommand = child.Original
				output = append(output, arg)
				current = current.Next
			}
		case "cmd":
			cmd := commands.Cmd{Values: []string{}, OriginalCommand: child.Original}
			current := child.Next
			for {
				if current == nil {
					break
				}
				cmd.Values = append(cmd.Values, current.Value)
				current = current.Next
			}
			output = append(output, cmd)
		case "copy":
			values := []string{}
			current := child.Next
			for {
				if current == nil {
					break
				}
				values = append(values, current.Value)
				current = current.Next
			}
			if len(values) == 2 {
				flags := readFlags(child.Flags)
				copy := commands.Copy{
					OriginalCommand: child.Original,
					OriginalSource:  originalSource,
					Source:          values[0],
					Stage:           flags.getOrDefault("--from", ""),
					Target:          values[1],
				}
				if chownVal, ok := flags.get("--chown"); ok {
					copy.UserFromLocalChown = &commands.User{Value: chownVal}
				}
				output = append(output, copy)
				continue
			}
			return output, fmt.Errorf("invalid COPY %q: %d", strings.Join(values, " "), child.StartLine)
		case "entrypoint":
			entrypoint := commands.Entrypoint{Values: []string{}, OriginalCommand: child.Original}
			current := child.Next
			for {
				if current == nil {
					break
				}
				entrypoint.Values = append(entrypoint.Values, current.Value)
				current = current.Next
			}
			output = append(output, entrypoint)
		case "env":
			extracted := []string{}
			current := child.Next
			for {
				if current == nil {
					break
				}
				extracted = append(extracted, current.Value)
				current = current.Next
			}
			if len(extracted)%2 != 0 {
				return nil, fmt.Errorf("the env at %d is not complete", child.StartLine)
			}
			for i := 0; i < len(extracted); i = i + 2 {
				//name, value := env.put(, )
				output = append(output, commands.Env{
					OriginalCommand: child.Original,
					Name:            extracted[i],
					Value:           extracted[i+1],
				})
			}
		case "expose":
			current := child.Next
			for {
				if current == nil {
					break
				}
				output = append(output, commands.Expose{RawValue: current.Value, OriginalCommand: child.Original})
				current = current.Next
			}
		case "from":
			values := []string{}
			current := child.Next
			for {
				if current == nil {
					break
				}
				values = append(values, current.Value)
				current = current.Next
			}
			// There are following variations of FROM:
			// - FROM source
			// - FROM source as stage
			if len(values) == 1 {
				output = append(output, commands.From{BaseImage: values[0],
					OriginalCommand: child.Original})
				continue
			}
			if len(values) == 3 {
				output = append(output, commands.From{BaseImage: values[0],
					StageName:       values[2],
					OriginalCommand: child.Original})
				continue
			}
			return output, fmt.Errorf("invalid FROM %q: %d", strings.Join(values, " "), child.StartLine)
		case "healthcheck":
			// ignore for now
			// TODO: these can be for sure used but at a higher level
		case "label":
			extracted := []string{}
			current := child.Next
			for {
				if current == nil {
					break
				}
				extracted = append(extracted, current.Value)
				current = current.Next
			}
			if len(extracted)%2 != 0 {
				return nil, fmt.Errorf("the label at %d is not complete", child.StartLine)
			}
			for i := 0; i < len(extracted); i = i + 2 {
				output = append(output, commands.Label{
					OriginalCommand: child.Original,
					Key:             extracted[i],
					Value:           extracted[i+1],
				})
			}
		case "maintainer":
			// ignore, it's deprectaed
		case "onbuild":
			// ignore for now
			// TODO: can these be used?
		case "run":
			current := child.Next
			for {
				if current == nil {
					break
				}
				output = append(output, commands.Run{
					OriginalCommand: child.Original,
					Command:         current.Value,
				})
				current = current.Next
			}
		case "shell":
			shell := commands.Shell{Commands: []string{}, OriginalCommand: child.Original}
			current := child.Next
			for {
				if current == nil {
					break
				}
				shell.Commands = append(shell.Commands, current.Value)
				current = current.Next
			}
			output = append(output, shell)
		case "stopsignal":
			// TODO: incorporate because the OS service manager can take advantage of this
		case "user":
			if child.Next == nil {
				return nil, fmt.Errorf("expected user value")
			}
			output = append(output, commands.User{Value: child.Next.Value, OriginalCommand: child.Original})
		case "volume":
			vols := commands.Volume{Values: []string{}, OriginalCommand: child.Original}
			current := child.Next
			for {
				if current == nil {
					break
				}
				vols.Values = append(vols.Values, current.Value)
				current = current.Next
			}
			output = append(output, vols)
		case "workdir":
			if child.Next == nil {
				return nil, fmt.Errorf("expected workdir value")
			}
			output = append(output, commands.Workdir{Value: child.Next.Value, OriginalCommand: child.Original})
		}
	}

	return output, nil
}
