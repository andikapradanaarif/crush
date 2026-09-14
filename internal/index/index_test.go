package index

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	abs := filepath.Join(root, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o755))
	require.NoError(t, os.WriteFile(abs, []byte(content), 0o644))
}

func TestTagFile_Go(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	src := `package handler

import (
	"fmt"
	"github.com/me/proj/internal/store"
)

type Handler struct {
	store *store.Store
}

func NewHandler(s *store.Store) *Handler { return &Handler{store: s} }

func (h *Handler) get() {}
`
	writeFile(t, root, "internal/api/handler.go", src)
	writeFile(t, root, "internal/store/store.go", "package store\n\ntype Store struct{}\n")
	writeFile(t, root, "go.mod", "module github.com/me/proj\n")

	exists := func(p string) bool {
		return p == "internal/api/handler.go" || p == "internal/store/store.go" ||
			p == "internal" || p == "internal/store" || p == "internal/api"
	}
	tags, refs, err := tagFile(root, "internal/api/handler.go", "github.com/me/proj", exists)
	require.NoError(t, err)

	names := map[string]tag{}
	for _, tg := range tags {
		names[tg.name] = tg
	}
	require.Contains(t, names, "Handler")
	require.Equal(t, "type", names["Handler"].kind)
	require.True(t, names["Handler"].exported)
	require.Contains(t, names, "NewHandler")
	require.True(t, names["NewHandler"].exported)
	require.Contains(t, names, "get")
	require.False(t, names["get"].exported)

	require.Equal(t, []string{"internal/store"}, refs)
}

func TestTagFile_Python(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, root, "api/views.py", `import models.user
from services.auth import verify

class UserView:
	def get(self): pass

def _helper(): pass

async def list_users(): pass
`)
	writeFile(t, root, "models/user.py", "class User: pass\n")
	writeFile(t, root, "services/auth.py", "def verify(): pass\n")

	exists := func(p string) bool {
		return p == "api/views.py" || p == "models/user.py" || p == "services/auth.py"
	}
	tags, refs, err := tagFile(root, "api/views.py", "", exists)
	require.NoError(t, err)

	names := map[string]tag{}
	for _, tg := range tags {
		names[tg.name] = tg
	}
	require.Contains(t, names, "UserView")
	require.Equal(t, "class", names["UserView"].kind)
	require.Contains(t, names, "get")
	require.Equal(t, "method", names["get"].kind)
	require.Contains(t, names, "list_users")
	require.Contains(t, names, "_helper")
	require.False(t, names["_helper"].exported)

	require.ElementsMatch(t, []string{"models/user.py", "services/auth.py"}, refs)
}

func TestTagFile_TypeScript(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, root, "src/routes/user.ts", `import { db } from '../db/client'
import express from 'express'

export interface UserRow { id: string }

export async function getUser(id: string) {}

const timeout = 5000

class UserService {
	async find(id: string) {
		doSomething()
	}
	if (x) { return }
}
`)
	writeFile(t, root, "src/db/client.ts", "export const db = {}\n")

	exists := func(p string) bool {
		return p == "src/routes/user.ts" || p == "src/db/client.ts" ||
			p == "src" || p == "src/db" || p == "src/routes"
	}
	tags, refs, err := tagFile(root, "src/routes/user.ts", "", exists)
	require.NoError(t, err)

	names := map[string]tag{}
	for _, tg := range tags {
		names[tg.name] = tg
	}
	require.Contains(t, names, "UserRow")
	require.Equal(t, "type", names["UserRow"].kind)
	require.True(t, names["UserRow"].exported)
	require.Contains(t, names, "getUser")
	require.Contains(t, names, "timeout")
	require.False(t, names["timeout"].exported)
	require.Contains(t, names, "find")
	require.Equal(t, "method", names["find"].kind)
	// Control-flow keywords must not leak in as methods.
	require.NotContains(t, names, "if")
	require.NotContains(t, names, "return")
	// Call sites must not tag as method definitions.
	require.NotContains(t, names, "doSomething")

	require.Equal(t, []string{"src/db/client.ts"}, refs)
}

func TestIndexRoundTrip(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dataDir := t.TempDir()

	writeFile(t, root, "go.mod", "module example.com/proj\n")
	writeFile(t, root, "main.go", `package main

import "example.com/proj/internal/api"

func main() {}
`)
	writeFile(t, root, "internal/api/handler.go", `package api

type Handler struct{}

func NewHandler() *Handler { return nil }
`)

	svc, err := Open(dataDir, root)
	require.NoError(t, err)
	defer svc.Close()

	ctx := context.Background()
	require.NoError(t, svc.EnsureIndexed(ctx))

	skel, err := svc.Skeleton(ctx, 500)
	require.NoError(t, err)
	require.Contains(t, skel, "internal/api")
	require.Contains(t, skel, "main.go")

	sym, err := svc.Symbol(ctx, "Handler", 500)
	require.NoError(t, err)
	require.Contains(t, sym, "internal/api/handler.go")

	sub, err := svc.Subtree(ctx, "internal/api", 500)
	require.NoError(t, err)
	require.Contains(t, sub, "NewHandler")

	// Staleness: edit the file — the next query must refresh it
	// lazily (refresh-before-render ordering), no manual refresh.
	writeFile(t, root, "internal/api/handler.go", `package api

type Handler struct{}

func NewHandler() *Handler { return nil }
func (h *Handler) ServeHTTP() {}
`)
	sub, err = svc.Subtree(ctx, "internal/api", 500)
	require.NoError(t, err)
	require.Contains(t, sub, "ServeHTTP")

	// New symbol in an already-indexed file: a Symbol miss triggers
	// the dirty-scan fallback and finds it without a re-walk.
	writeFile(t, root, "internal/api/handler.go", `package api

type Handler struct{}

func NewHandler() *Handler { return nil }
func (h *Handler) ServeHTTP() {}
func Extra() {}
`)
	sym, err = svc.Symbol(ctx, "Extra", 500)
	require.NoError(t, err)
	require.Contains(t, sym, "internal/api/handler.go")

	// Go dir refs must survive lazy re-tagging: edit main.go, let a
	// query refresh it, then Handler's referrers still include it.
	writeFile(t, root, "main.go", `package main

import "example.com/proj/internal/api"

func main() { api.NewHandler() }
`)
	_, err = svc.Symbol(ctx, "main", 500)
	require.NoError(t, err)
	sym, err = svc.Symbol(ctx, "Handler", 500)
	require.NoError(t, err)
	require.Contains(t, sym, "main.go")

	// Root listing: "." and "/" address the project root, not an
	// empty result.
	sub, err = svc.Subtree(ctx, ".", 500)
	require.NoError(t, err)
	require.Contains(t, sub, "main.go")
	sub, err = svc.Subtree(ctx, "/", 500)
	require.NoError(t, err)
	require.Contains(t, sub, "main.go")

	// Oversized files are recorded but not tagged: a fresh service
	// re-walks and big.go appears in listings without symbols.
	writeFile(t, root, "big.go", "package main\n// "+strings.Repeat("x", 300*1024))
	svc2, err := Open(dataDir, root)
	require.NoError(t, err)
	defer svc2.Close()
	require.NoError(t, svc2.EnsureIndexed(ctx))
	sub, err = svc2.Subtree(ctx, ".", 500)
	require.NoError(t, err)
	require.Contains(t, sub, "big.go")
}
