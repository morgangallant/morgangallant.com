package main

import (
	"bytes"
	"cmp"
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/feeds"
	"github.com/joho/godotenv"
	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/parser"
	"go.abhg.dev/goldmark/frontmatter"
)

var logLevels = map[string]slog.Level{
	"DEBUG": slog.LevelDebug,
	"INFO":  slog.LevelInfo,
	"WARN":  slog.LevelWarn,
	"ERROR": slog.LevelError,
}

var (
	production_once   sync.Once
	production_cached bool
)

func production() bool {
	production_once.Do(func() {
		hostname, err := os.Hostname()
		if err != nil {
			panic(err)
		}
		production_cached = runtime.GOOS != "darwin" &&
			hostname != "mbp"
	})
	return production_cached
}

func main() {
	_ = godotenv.Load()

	var logLevel slog.Level
	if level, ok := logLevels[os.Getenv("LOG_LEVEL")]; ok {
		logLevel = level
	}

	var logger *slog.Logger
	if production() {
		logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
			Level: logLevel,
		}))
	} else {
		logLevel = slog.LevelDebug
		logger = slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
			Level: logLevel,
		}))
	}

	rctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	if err := run(rctx, logger, cancel); err != nil {
		logger.Error("encountered top-level error", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

//go:embed static
var staticFiles embed.FS

func run(ctx context.Context, logger *slog.Logger, shutdown context.CancelFunc) error {
	templates, err := loadTemplates()
	if err != nil {
		return fmt.Errorf("loading templates: %w", err)
	}

	posts, err := loadPosts()
	if err != nil {
		return fmt.Errorf("loading posts: %w", err)
	}

	slugIndex := make(map[string]int, len(posts))
	for i, p := range posts {
		if _, ok := slugIndex[p.Slug]; ok {
			return fmt.Errorf("duplicate post %s found, shouldn't be possible", p.Slug)
		}
		slugIndex[p.Slug] = i
	}

	mux := http.NewServeMux()

	if err := registerPublicDir(mux); err != nil {
		return fmt.Errorf("registering public files with mux: %w", err)
	}

	if err := templates.registerHandler(mux, "GET /", "index", func(_ *http.Request) (any, error) {
		type innerType struct {
			RecentPosts []*post
		}
		return templateData[innerType]{
			Inner: innerType{
				RecentPosts: posts[:min(len(posts), 3)],
			},
		}, nil
	}); err != nil {
		return fmt.Errorf("register index handler: %w", err)
	}

	if err := templates.registerHandler(mux, "GET /blog", "blog", func(_ *http.Request) (any, error) {
		type innerType struct {
			Posts []*post
		}
		return templateData[innerType]{
			Inner: innerType{
				Posts: posts,
			},
			Subtitle: "Blog",
		}, nil
	}); err != nil {
		return fmt.Errorf("registering blog handler: %w", err)
	}

	if err := templates.registerHandler(mux, "GET /blog/{slug}", "blog_post", func(r *http.Request) (any, error) {
		idx, ok := slugIndex[r.PathValue("slug")]
		if !ok {
			return nil, errNotFound
		}
		p := posts[idx]
		return templateData[*post]{
			Inner:    p,
			Subtitle: p.Title,
		}, nil
	}); err != nil {
		return fmt.Errorf("registering blog post handler: %w", err)
	}

	if err := templates.registerHandler(mux, "GET /uses", "uses", func(_ *http.Request) (any, error) {
		type innerType struct{}
		return templateData[innerType]{
			Subtitle: "Uses",
		}, nil
	}); err != nil {
		return fmt.Errorf("registering uses page: %w", err)
	}

	feed := &feeds.Feed{
		Title:       "Morgan Gallant's blog",
		Link:        &feeds.Link{Href: "https://morgangallant.com/blog"},
		Description: "Ramblings about technology, software... and probably some other stuff too",
		Author:      &feeds.Author{Name: "Morgan Gallant", Email: "morgan@morgangallant.com"},
		Created:     time.Now(),
	}
	for _, p := range posts {
		feed.Items = append(feed.Items, &feeds.Item{
			Title:   p.Title,
			Link:    &feeds.Link{Href: "https://morgangallant.com/blog/" + p.Slug},
			Author:  &feeds.Author{Name: "Morgan Gallant", Email: "morgan@morgangallant.com"},
			Created: p.PublishedAt,
		})
	}
	rss, err := feed.ToRss()
	if err != nil {
		return fmt.Errorf("creating rss feed: %w", err)
	}

	mux.HandleFunc("GET /feed.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		if n, err := w.Write([]byte(rss)); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		} else if n != len(rss) {
			http.Error(w, "short write", http.StatusInternalServerError)
			return
		}
	})

	var port uint16 = 8080
	if portStr, ok := os.LookupEnv("PORT"); ok {
		parsed, err := strconv.ParseUint(portStr, 10, 16)
		if err != nil {
			return fmt.Errorf("parsing port string %s: %w", portStr, err)
		}
		port = uint16(parsed)
	}

	httpAddr := fmt.Sprintf("0.0.0.0:%d", port)
	httpSrv := &http.Server{
		Addr:         httpAddr,
		Handler:      mux,
		ReadTimeout:  time.Second,
		WriteTimeout: time.Second * 10,
	}
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server returned, shutting down", slog.String("error", err.Error()))
			shutdown()
			return
		}
		logger.Info("http server shutdown")
	}()
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), time.Second*3)
		defer cancel()
		if err := httpSrv.Shutdown(sctx); err != nil {
			logger.Error("failed to shutdown http server", slog.String("error", err.Error()))
		}
	}()
	logger.Info("started http server", slog.String("addr", httpAddr))

	<-ctx.Done()
	return nil
}

func registerPublicDir(mux *http.ServeMux) error {
	const dirPath = "static/public"
	return fs.WalkDir(
		staticFiles,
		dirPath,
		func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			} else if d.IsDir() {
				return nil
			}
			trimmed := strings.TrimPrefix(path, dirPath)
			mux.HandleFunc("GET "+trimmed, func(w http.ResponseWriter, r *http.Request) {
				http.ServeFileFS(w, r, staticFiles, path)
			})
			return nil
		},
	)
}

type post struct {
	Title       string
	PublishedAt time.Time
	Slug        string
	Content     template.HTML
}

var (
	mdparser = goldmark.New(
		goldmark.WithExtensions(&frontmatter.Extender{}),
	)
	bmPolicy = bluemonday.UGCPolicy()
)

func loadPost(path string) (*post, error) {
	f, err := staticFiles.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	defer f.Close()

	content, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("reading content: %w", err)
	}

	var buf bytes.Buffer

	ctx := parser.NewContext()
	if err := mdparser.Convert(content, &buf, parser.WithContext(ctx)); err != nil {
		return nil, fmt.Errorf("converting markdown: %w", err)
	}

	var meta struct {
		Title     string `yaml:"title"`
		Published string `yaml:"published"`
	}
	if err := frontmatter.Get(ctx).Decode(&meta); err != nil {
		return nil, fmt.Errorf("extracting frontmatter: %w", err)
	}

	parsed, err := time.Parse("Jan 02 2006 MST", meta.Published)
	if err != nil {
		return nil, fmt.Errorf("parsing post timestamp '%s': %w", meta.Published, err)
	}

	return &post{
		Title:       meta.Title,
		PublishedAt: parsed,
		Slug:        strings.TrimSuffix(filepath.Base(path), ".md"),
		Content:     template.HTML(bmPolicy.Sanitize(buf.String())),
	}, nil
}

func loadPosts() ([]*post, error) {
	const dirPath = "static/posts"
	files, err := fs.ReadDir(staticFiles, dirPath)
	if err != nil {
		return nil, fmt.Errorf("read dir %s: %w", dirPath, err)
	}

	var posts []*post
	for _, f := range files {
		p := filepath.Join(dirPath, f.Name())
		loaded, err := loadPost(p)
		if err != nil {
			return nil, fmt.Errorf("loading %s: %w", f.Name(), err)
		}
		posts = append(posts, loaded)
	}

	slices.SortFunc(posts, func(a, b *post) int {
		return cmp.Compare(a.PublishedAt.Unix(), b.PublishedAt.Unix()) * -1
	})

	return posts, nil
}

type templateSet struct {
	tmpls map[string]*template.Template
}

func (ts *templateSet) exec(w io.Writer, id string, data any) error {
	t, ok := ts.tmpls[id]
	if !ok {
		return fmt.Errorf("missing template %s", id)
	}
	return t.ExecuteTemplate(w, "base", data)
}

type templateDataFunc func(*http.Request) (any, error)

type templateData[T any] struct {
	Inner    T
	Subtitle string
}

var errNotFound = errors.New("not found")

func (ts *templateSet) registerHandler(
	mux *http.ServeMux,
	pattern, tmpl string,
	dataFn templateDataFunc,
) error {
	if _, ok := ts.tmpls[tmpl]; !ok {
		return fmt.Errorf("missing template %s", tmpl)
	}
	mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		var data any
		if dataFn != nil {
			d, err := dataFn(r)
			if errors.Is(err, errNotFound) {
				http.NotFound(w, r)
				return
			} else if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			data = d
		}
		if err := ts.exec(w, tmpl, data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	})
	return nil
}

const templateExt = ".tmpl.html"

func loadTemplates() (*templateSet, error) {
	const dirPath = "static/templates"
	sub, err := fs.Sub(staticFiles, dirPath)
	if err != nil {
		return nil, fmt.Errorf("sub-fs load template dir: %w", err)
	}

	baseName := "base" + templateExt
	if _, err := fs.Stat(sub, baseName); errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("missing base template %s", baseName)
	} else if err != nil {
		return nil, fmt.Errorf("checking for base template %s: %w", baseName, err)
	}

	tmpls := make(map[string]*template.Template)
	if err := fs.WalkDir(sub, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() || !strings.HasSuffix(name, templateExt) || name == baseName {
			return nil
		}
		id := strings.TrimSuffix(path, templateExt)
		tmpl, err := template.New(id).ParseFS(
			sub,
			[]string{path, baseName}...,
		)
		if err != nil {
			return fmt.Errorf("creating template at %s: %w", path, err)
		}
		tmpls[id] = tmpl
		return nil
	}); err != nil {
		return nil, fmt.Errorf("walking templates dir: %w", err)
	}

	return &templateSet{tmpls}, nil
}
