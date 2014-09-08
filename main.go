package main

import (
	"flag"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"regexp"
	"sync"

	"github.com/kylelemons/go-gypsy/yaml"
	"github.com/shawnps/gr"
	"github.com/shawnps/rt"
	"github.com/shawnps/sp"

	"appengine"
	"appengine/datastore"
)

var (
	port        = flag.String("p", "8000", "Port number (default 8000)")
	configFile  = flag.String("c", "config.yml", "Config file (default config.yml)")
	entriesPath = flag.String("f", "entries.json", "Path to JSON storage file (default entries.json)")
)

type Entry struct {
	Id       string
	Title    string
	Link     string
	ImageURL url.URL
	Type     string
}

func parseYAML() (rtKey, grKey, grSecret string, err error) {
	config, err := yaml.ReadFile(*configFile)
	if err != nil {
		return
	}
	rtKey, err = config.Get("rt")
	if err != nil {
		return
	}
	grKey, err = config.Get("gr.key")
	if err != nil {
		return
	}
	grSecret, err = config.Get("gr.secret")
	if err != nil {
		return
	}

	return rtKey, grKey, grSecret, nil
}

func entryKey(c appengine.Context) *datastore.Key {
	return datastore.NewKey(c, "Entry", "default_entry", 0, nil)
}

func insertEntry(title, link, mediaType, imageURL string, r *http.Request) error {
	url, err := url.Parse(imageURL)
	if err != nil {
		return err
	}
	e := Entry{Title: title, Link: link, ImageURL: *url, Type: mediaType}
	c := appengine.NewContext(r)
	key := datastore.NewIncompleteKey(c, "Entry", entryKey(c))
	_, err = datastore.Put(c, key, &e)

	return err
}

func truncate(s, suf string, l int) string {
	if len(s) < l {
		return s
	}
	return s[:l] + suf
}

// Search Rotten Tomatoes, Goodreads, and Spotify.
func Search(q string, rtClient rt.RottenTomatoes, grClient gr.Goodreads, spClient sp.Spotify) (m []rt.Movie, g gr.GoodreadsResponse, s sp.SearchAlbumsResponse) {
	var wg sync.WaitGroup
	wg.Add(3)
	go func(q string) {
		defer wg.Done()
		movies, err := rtClient.SearchMovies(q)
		if err != nil {
			fmt.Println("ERROR (rt): ", err.Error())
		}
		for _, mov := range movies {
			mov.Title = truncate(mov.Title, "...", 60)
			m = append(m, mov)
		}
	}(q)
	go func(q string) {
		defer wg.Done()
		books, err := grClient.SearchBooks(q)
		if err != nil {
			fmt.Println("ERROR (gr): ", err.Error())
		}
		for i, w := range books.Search.Works {
			w.BestBook.Title = truncate(w.BestBook.Title, "...", 60)
			books.Search.Works[i] = w
		}
		g = books
	}(q)
	go func(q string) {
		defer wg.Done()
		albums, err := spClient.SearchAlbums(q)
		if err != nil {
			fmt.Println("ERROR (sp): ", err.Error())
		}
		for i, a := range albums.Albums {
			a.Name = truncate(a.Name, "...", 60)
			albums.Albums[i] = a
		}
		s = albums
	}(q)
	wg.Wait()
	return m, g, s
}

func HomeHandler(w http.ResponseWriter, r *http.Request) {
	t, err := template.New("index.html").ParseFiles("templates/index.html", "templates/base.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Render the template
	err = t.ExecuteTemplate(w, "base", map[string]interface{}{"Page": "home"})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func SearchHandler(w http.ResponseWriter, r *http.Request, query string) {
	rtKey, grKey, grSecret, err := parseYAML()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rtClient := rt.RottenTomatoes{rtKey}
	grClient := gr.Goodreads{grKey, grSecret}
	spClient := sp.Spotify{}
	m, g, s := Search(query, rtClient, grClient, spClient)
	// Since spotify: URIs are not trusted, have to pass a
	// URL function to the template to use in hrefs
	funcMap := template.FuncMap{
		"URL": func(q string) template.URL { return template.URL(query) },
	}
	t, err := template.New("search.html").Funcs(funcMap).ParseFiles("templates/search.html", "templates/base.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Render the template
	err = t.ExecuteTemplate(w, "base", map[string]interface{}{"Movies": m, "Books": g, "Albums": s.Albums})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func SaveHandler(w http.ResponseWriter, r *http.Request) {
	t := r.FormValue("title")
	l := r.FormValue("link")
	m := r.FormValue("media_type")
	url := r.FormValue("image_url")
	err := insertEntry(t, l, m, url, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/list", http.StatusFound)
}

func ListHandler(w http.ResponseWriter, r *http.Request) {
	e, err := readEntries()
	if err != nil {
		http.Error(w, fmt.Sprintf("Error reading entries: %v", err), http.StatusInternalServerError)
		return
	}
	m := buildEntryMap(e)
	// Create and parse Template
	t, err := template.New("list.html").ParseFiles("templates/list.html", "templates/base.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Render the template
	t.ExecuteTemplate(w, "base", map[string]interface{}{"Entries": m, "Page": "list"})
}

func RemoveHandler(w http.ResponseWriter, r *http.Request) {
	i := r.FormValue("id")
	err := removeEntry(i)
	if err != nil {
		http.Error(w, fmt.Sprintf("Error reading entries: %v", err), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/list", http.StatusFound)
}

var validSearchPath = regexp.MustCompile("^/search/(.*)$")

func makeSearchHandler(fn func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m := validSearchPath.FindStringSubmatch(r.URL.Path)
		if m == nil {
			http.NotFound(w, r)
			return
		}
		fn(w, r, m[1])
	}
}

func init() {
	http.HandleFunc("/", HomeHandler)
}

//func main() {
//	flag.Parse()
//	http.HandleFunc("/", HomeHandler)
//	http.HandleFunc("/search/", makeSearchHandler(SearchHandler))
//	http.HandleFunc("/save", SaveHandler)
//	http.HandleFunc("/list", ListHandler)
//	http.HandleFunc("/remove", RemoveHandler)
//	fmt.Println("Running on localhost:" + *port)
//
//	log.Fatal(http.ListenAndServe(":"+*port, nil))
//}
