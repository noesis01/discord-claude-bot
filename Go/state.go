package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"sync"
)

// Conv 는 채널(스레드·DM)별 Claude 세션 ID 와 작업 디렉터리. state.json 형식은 Python 버전과 같다.
type Conv struct {
	SessionID *string `json:"session_id"`
	Cwd       string  `json:"cwd"`
}

func (c *Conv) Session() string {
	if c.SessionID == nil {
		return ""
	}
	return *c.SessionID
}

func (c *Conv) SetSession(id string) {
	if id == "" {
		c.SessionID = nil
	} else {
		c.SessionID = &id
	}
}

// State: 디스크에 저장되는 대화별 상태. 여러 채널의 처리가 동시에 돌므로 mu 로 보호한다.
type State struct {
	mu         sync.Mutex
	path       string
	defaultCwd string
	data       map[string]*Conv
}

func loadState(path, defaultCwd string) (*State, error) {
	st := &State{path: path, defaultCwd: defaultCwd, data: map[string]*Conv{}}
	b, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(b, &st.data); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return st, nil
}

// Get 은 대화 상태를 돌려준다 (없으면 기본값으로 만든다). 필드를 읽고 쓸 때는 Do 를 쓴다.
func (st *State) Get(key string) *Conv {
	st.mu.Lock()
	defer st.mu.Unlock()
	c, ok := st.data[key]
	if !ok {
		c = &Conv{Cwd: st.defaultCwd}
		st.data[key] = c
	}
	return c
}

// Do 는 잠금 안에서 f 를 실행한다 (대화 상태 읽기·수정).
func (st *State) Do(f func()) {
	st.mu.Lock()
	defer st.mu.Unlock()
	f()
}

func (st *State) Save() error {
	st.mu.Lock()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	err := enc.Encode(st.data)
	st.mu.Unlock()
	if err != nil {
		return err
	}
	tmp := strings.TrimSuffix(st.path, ".json") + ".tmp"
	if err := os.WriteFile(tmp, bytes.TrimRight(buf.Bytes(), "\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, st.path)
}
