package services

import (
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/pkg/errors"
)

// Mod struct represents modification of source file.
type Mod struct {
	Type  string `json:"type"`
	Path  string `json:"path"`
	Extra string `json:"extra"`
	Name  string `json:"name"`
}

// Source struct represents torrent file source.
// Source may have additional modification.
type Source struct {
	Type     string `json:"type"`
	Name     string `json:"name"`
	InfoHash string `json:"info_hash"`
	Path     string `json:"path"`
	Token    string `json:"token"`
	ApiKey   string `json:"api_key"`
	Query    string `json:"query"`
	Mod      *Mod
}

func (s *Source) GetKey() string {
	key := s.InfoHash + s.Type
	if s.Mod != nil {
		key = key + s.Path + s.Mod.Type + s.Mod.Extra
	}
	return key
}

func (s *Source) GetEdgeName() string {
	if s.Mod != nil {
		return s.Mod.Name
	}
	return s.Name
}

func checkHash(hash string) bool {
	match, _ := regexp.MatchString("[0-9a-f]{5,40}", hash)
	return match
}

func (s *URLParser) extractMod(path string) (string, *Mod, error) {
	if !strings.Contains(path, "~") {
		return path, nil, nil
	}
	index := strings.LastIndex(path, "~")
	first := path[:index]
	last := path[index+1:]
	newPath := first
	p := strings.SplitN(last, "/", 2)
	t := p[0]
	ee := strings.SplitN(t, ":", 2)
	e := ""
	if len(ee) > 1 {
		e = ee[1]
		t = ee[0]
	}
	// if t == "" {
	// 	return "", nil, errors.New("Empty mod name")
	// }
	exist := false
	name := ""
	for _, v := range s.configs.GetMods() {
		if t == v {
			exist = true
			name = s.configs.GetMod(v).Name
			break
		}
	}
	if !exist {
		return path, nil, nil
	}
	modPath := "/"
	if len(p) > 1 {
		modPath += p[1]
	}
	modPath = filepath.Clean(modPath)
	m := &Mod{
		Type:  t,
		Path:  modPath,
		Extra: e,
		Name:  name,
	}
	return newPath, m, nil
}

type URLParser struct {
	configs *ConnectionsConfig
}

func NewURLParser(c *ConnectionsConfig) *URLParser {
	return &URLParser{
		configs: c,
	}
}

// ParseURL extracts information about source and additional modifiacation of it
func (s *URLParser) Parse(url *url.URL) (*Source, error) {
	urlPath := url.Path
	if urlPath == "" {
		return nil, errors.New("Empty url")
	}
	p := strings.SplitN(urlPath[1:], "/", 2)
	hash := p[0]
	if hash == "" {
		return nil, errors.New("Empty hash")
	}
	sourceType := "default"
	for _, v := range s.configs.GetMods() {
		if hash == v {
			sourceType = v
			break
		}
	}
	sourceName := s.configs.GetMod(sourceType).Name
	if sourceType == "default" && !checkHash(hash) {
		return nil, errors.New(fmt.Sprintf("Wrong hash=%s", hash))
	}
	path := "/"
	if len(p) > 1 {
		path += p[1]
	}
	// path = filepath.Clean(path)
	newPath, mod, err := s.extractMod(path)
	if err != nil {
		return nil, errors.Wrapf(err, "Failed to extract mod from path=%s", path)
	}
	ss := &Source{
		InfoHash: hash,
		Path:     newPath,
		Token:    url.Query().Get("token"),
		ApiKey:   url.Query().Get("api-key"),
		Query:    url.RawQuery,
		Type:     sourceType,
		Name:     sourceName,
		Mod:      mod,
	}
	return ss, nil
}
