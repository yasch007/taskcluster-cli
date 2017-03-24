package apis

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/format"
	"log"
	"reflect"
	"sort"
	"sync"

	got "github.com/taskcluster/go-got"
	"github.com/taskcluster/taskcluster-cli/apis/definitions"
)

func GenerateServices(manifestURL, servicesVar, schemasVar string) ([]byte, error) {
	// synchronization objects
	mutex := &sync.Mutex{}
	wg := &sync.WaitGroup{}

	// go-got is thread-safe by virtue of only reading from the shared object
	// and initializing anything within the scope of a function.
	g := got.New()

	gen := &generator{}

	gen.Print("package apis\n")
	gen.Print("// Code generated by fetch-apis; DO NOT EDIT\n")
	gen.Print("\n")
	gen.Print("import \"github.com/taskcluster/taskcluster-cli/apis/definitions\"\n")
	gen.Print("\n")

	// Fetch API manifest
	res, err := g.Get(manifestURL).Send()
	if err != nil {
		log.Fatalln("error: failed to fetch api manifest: ", err)
	}
	// Parse API manifest
	var manifest map[string]string
	if err = json.Unmarshal(res.Body, &manifest); err != nil {
		log.Fatalln("error: failed to parse api manifest: ", err)
	}

	log.Println("Fetching Services:")
	services := make(map[string]definitions.Service)
	for name, referenceURL := range manifest {
		wg.Add(1)
		go func(n string, u string) {
			s := fetchService(g, n, u)

			mutex.Lock()
			services[n] = s
			mutex.Unlock()
			wg.Done()
		}(name, referenceURL)
	}
	wg.Wait()

	gen.Printf("var %s = ", servicesVar)
	gen.PrettyPrint(services)
	gen.Print("\n")

	// Fetch all schemas
	log.Println("Fetching Schemas:")
	schemas := make(map[string]string, 0)
	urls := make(map[string]bool, 0)

	// addSchema is the function that determines if a schema url needs to be
	// fetched and starts the goroutine to fetch it if needed.
	addSchema := func(url string) {
		if url == "" || urls[url] {
			return
		}

		// map access/modification is not thread-safe.
		urls[url] = true
		wg.Add(1)
		go func() {
			s := fetchSchema(g, url)

			mutex.Lock()
			schemas[url] = s
			mutex.Unlock()
			wg.Done()
		}()
	}
	for _, s := range services {
		for _, e := range s.Entries {
			addSchema(e.Input)
			addSchema(e.Output)
		}
	}
	wg.Wait()

	gen.Printf("var %s = ", schemasVar)
	gen.PrettyPrint(schemas)
	gen.Print("\n")

	return gen.Format()
}

// fetchService uses go-got to fetch the definition of a service and parses it
// into a usable go object.
func fetchService(g *got.Got, name string, url string) definitions.Service {
	log.Println(" - fetching", name)
	// Fetch reference
	res, err := g.Get(url).Send()
	if err != nil {
		log.Fatalln("error: failed to fetch API ", name, ": ", err)
	}
	// Parse reference
	var s definitions.Service
	if err := json.Unmarshal(res.Body, &s); err != nil {
		log.Fatalln("error: failed parse API ", name, ": ", err)
	}
	return s
}

// fetchSchema uses go-got to fetch the schema of an input or output and ensures
// that it parses as valid JSON.
func fetchSchema(g *got.Got, url string) string {
	log.Println(" -", url)
	res, err := g.Get(url).Send()
	if err != nil {
		log.Fatalln("error: failed to fetch ", url, ": ", err)
	}
	// Test that we can parse the JSON schema (otherwise it's invalid)
	var i interface{}
	if err := json.Unmarshal(res.Body, &i); err != nil {
		log.Fatalln("error: failed to parse ", url, ": ", err)
	}
	return string(res.Body)
}

// generator holds a buffer of the output that will be generated.
type generator struct {
	buf bytes.Buffer
}

// Write writes arbitrary bytes to the buffer. This meets the requirements for
// the io.Writer interface.
func (g *generator) Write(p []byte) (n int, err error) {
	return g.buf.Write(p)
}

// Printf prints the given format+args to the buffer.
func (g *generator) Printf(format string, args ...interface{}) {
	fmt.Fprintf(&g.buf, format, args...)
}

// Print prints the given a to the buffer.
func (g *generator) Print(a ...interface{}) {
	fmt.Fprint(&g.buf, a...)
}

// PrettyPrint pretty-prints arbitrary data.
//
// There are special rules for some composite types to ensure we have verbose
// output, but simple types such as strings and numbers are printed using the
// built-in `%#v` format filter.
func (g *generator) PrettyPrint(data interface{}) {
	v := reflect.ValueOf(data)
	t := v.Type()

	switch v.Kind() {
	case reflect.Array, reflect.Slice:
		g.Printf("%s", t.String())
		if v.Kind() == reflect.Slice && v.IsNil() {
			g.Printf("(nil)")
			break
		}
		g.Print("{\n")
		for i := 0; i < v.Len(); i++ {
			g.PrettyPrint(v.Index(i).Interface())
			g.Print(",\n")
		}
		g.Print("}")
	case reflect.Struct:
		g.Printf("%s{\n", t.String())
		for i := 0; i < v.NumField(); i++ {
			g.Printf("%s: ", t.Field(i).Name)
			g.PrettyPrint(v.Field(i).Interface())
			g.Print(",\n")
		}
		g.Print("}")
	case reflect.Map:
		g.Printf("%s{\n", t.String())
		keys := v.MapKeys()
		if len(keys) == 0 {
			g.Print("}")
			break
		}
		// Because go's maps don't do stable ordering, we manually sort the maps
		// where keys are strings (our only usecase so far) to ensure we get
		// consistent outputs and reduce potential diffs.
		if t.Key().Kind() == reflect.String {
			sortedK := make([]string, 0, len(keys))
			for _, k := range keys {
				sortedK = append(sortedK, k.String())
			}
			sort.Strings(sortedK)
			for i := range sortedK {
				k := reflect.ValueOf(sortedK[i])
				g.Printf("%#v: ", sortedK[i])
				g.PrettyPrint(v.MapIndex(k).Interface())
				g.Print(",\n")
			}
		} else {
			// If the keys are not strings, we don't sort them for now.
			for i := 0; i < v.Len(); i++ {
				k := keys[i]
				g.Printf("%#v: ", k)
				g.PrettyPrint(v.MapIndex(k).Interface())
				g.Print(",\n")
			}
		}
		g.Print("}")
	default:
		g.Printf("%#v", v.Interface())
	}
}

// Format returns the formated contents of the generator's buffer.
func (g *generator) Format() ([]byte, error) {
	return format.Source(g.buf.Bytes())
}

// String returns a string representation of the generator's buffer.
func (g *generator) String() string {
	return g.buf.String()
}
