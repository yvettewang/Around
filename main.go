package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"reflect"
	"strconv"
	"strings"

	jwtmiddleware "github.com/auth0/go-jwt-middleware"
	"github.com/dgrijalva/jwt-go"
	"github.com/gorilla/mux"

	"cloud.google.com/go/bigtable"
	"cloud.google.com/go/storage"

	"github.com/pborman/uuid"
	elastic "gopkg.in/olivere/elastic.v3"
)

type Location struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

type Post struct {
	User     string   `json:"user"`
	Message  string   `json:"message"`
	Location Location `json:"location"`
	Url      string   `json:"url"`
}

const (
	INDEX       = "around"
	TYPE        = "post"
	DISTANCE    = "200km"
	ES_URL      = "http://35.238.255.23:9200"
	BUCKET_NAME = "post-images-237801"
	PROJECT_ID  = "around-237801"
	BT_INSTANCE = "around-post"
)

// Variable with capital letter is exported, like public

var mySigningKey = []byte("long-secret")

func main() {
	// Create a client
	client, err := elastic.NewClient(elastic.SetURL(ES_URL), elastic.SetSniff(false))
	if err != nil {
		panic(err)
		return
	}

	// Use the IndexExists service to check if a specified index exists.
	exists, err := client.IndexExists(INDEX).Do()
	if err != nil {
		panic(err)
	}
	if !exists {
		// Create a new index.
		mapping := `{
			"mappings":{
				"post":{
					"properties":{
						"location":{
							"type":"geo_point"
						}
					}
				}
			}
		}`
		_, err := client.CreateIndex(INDEX).Body(mapping).Do()
		if err != nil {
			// Handle error
			panic(err)
		}
	}

	fmt.Println("Started-Service")

	r := mux.NewRouter()

	var jwtMiddleware = jwtmiddleware.New(jwtmiddleware.Options{
		ValidationKeyGetter: func(token *jwt.Token) (interface{}, error) {
			return mySigningKey, nil
		},
		SigningMethod: jwt.SigningMethodHS256,
	})
	//Recall that: Original version: http.HandlerFunc("/post", handlerPost)
	// Here we use jwtMiddleware before executing http.HanderFunc(...),
	// since jwtMiddleware can check if client's token is valid or not.
	// If invalid, we will reject them.
	r.Handle("/post", jwtMiddleware.Handler(http.HandlerFunc(handlerPost))).Methods("POST")    // endpoint and doPost
	r.Handle("/search", jwtMiddleware.Handler(http.HandlerFunc(handlerSearch))).Methods("GET") // endpoint and doPost

	// Users haven't got their tokens yet, no need to use jwtMiddleware for /login and /signup.
	r.Handle("/login", http.HandlerFunc(loginHandler)).Methods("POST")
	r.Handle("/signup", http.HandlerFunc(signupHandler)).Methods("POST")

	http.Handle("/", r)

	log.Fatal(http.ListenAndServe(":8080", nil))
}

// {
// 	 "user": "john",
// 	 "message": "test",
// 	 "location": {
// 		"lat": 37,
// 		"lon": -120
// 	 }
// }
// json body's format should be exactly matched with the Post struct's format

func handlerPost(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Header", "Content-Type,Authorization")

	user := r.Context().Value("user")
	claims := user.(*jwt.Token).Claims
	username := claims.(jwt.MapClaims)["username"]

	// 32 << 20 is the maxMemory param for ParseMultipartForm
	// After you call ParseMultipartForm, the file will be saved in the server memory with maxMemory size.
	// If the file size is larger than maxMemory, the rest of the data will be saved in a system temporary file.
	r.ParseMultipartForm(32 << 20) // 32MB

	// Parse form data
	fmt.Printf("Received one post request %s\n", r.FormValue("message"))
	lat, _ := strconv.ParseFloat(r.FormValue("lat"), 64)
	lon, _ := strconv.ParseFloat(r.FormValue("lon"), 64)

	p := &Post{
		User:    username.(string),
		Message: r.FormValue("message"),
		Location: Location{
			Lat: lat,
			Lon: lon,
		},
	}

	id := uuid.New()

	file, _, err := r.FormFile("image")
	if err != nil {
		http.Error(w, "GCS is not setup", http.StatusInternalServerError)
		fmt.Printf("GCS is not setup %v\n", err)
		panic(err)
	}
	defer file.Close()

	ctx := context.Background() // context reads the credential then can access the data stored in GCS

	_, attrs, err := saveToGCS(ctx, file, BUCKET_NAME, id)

	if err != nil {
		http.Error(w, "GCS is not setup", http.StatusInternalServerError)
		fmt.Printf("GCS is not setup %v\n", err)
		panic(err)
	}

	p.Url = attrs.MediaLink

	// save to elastic search
	saveToES(p, id)

	// save to BigTable
	// saveToBigTable(ctx, p, id, PROJECT_ID, BT_INSTANCE)
}

func saveToGCS(ctx context.Context, r io.Reader, bucketName string, name string) (*storage.ObjectHandle, *storage.ObjectAttrs, error) {
	// create a new client
	client, err := storage.NewClient(ctx)
	if err != nil {
		return nil, nil, err
	}
	// create a bucket instance
	bucket := client.Bucket(bucketName)
	// check if the bucket created exists
	if _, err := bucket.Attrs(ctx); err != nil {
		return nil, nil, err
	}
	// write files to the bucket
	obj := bucket.Object(name)
	wc := obj.NewWriter(ctx)
	if _, err := io.Copy(wc, r); err != nil {
		return nil, nil, err
	}
	if err := wc.Close(); err != nil {
		return nil, nil, err
	}
	// set Access Control List
	if err := obj.ACL().Set(ctx, storage.AllUsers, storage.RoleReader); err != nil {
		return nil, nil, err
	}
	// get url to the file
	attrs, err := obj.Attrs(ctx)
	fmt.Printf("Post is saved to GCS: %s\n", attrs.MediaLink)
	return obj, attrs, err
}

func saveToES(p *Post, id string) {
	es_client, err := elastic.NewClient(elastic.SetURL(ES_URL),
		elastic.SetSniff(false))
	if err != nil {
		panic(err)
	}
	_, err = es_client.Index().
		Index(INDEX).
		Type(TYPE).
		Id(id).
		BodyJson(p).
		Refresh(true).
		Do()
	if err != nil {
		panic(err)
	}
	fmt.Printf("Post is saved to index: %s\n", p.Message)
}

func saveToBigTable(ctx context.Context, p *Post, id string, PROJECT_ID string, BT_INSTANCE string) {
	btClient, err := bigtable.NewClient(ctx, PROJECT_ID, BT_INSTANCE)
	if err != nil {
		panic(err)
		return
	}

	tbl := btClient.Open("post")
	mut := bigtable.NewMutation()
	t := bigtable.Now() // timestamp

	mut.Set("post", "user", t, []byte(p.User)) // convert data to byte array
	mut.Set("post", "message", t, []byte(p.Message))
	mut.Set("location", "lat", t, []byte(strconv.FormatFloat(p.Location.Lat, 'f', -1, 64)))
	mut.Set("location", "lon", t, []byte(strconv.FormatFloat(p.Location.Lon, 'f', -1, 64)))

	err = tbl.Apply(ctx, id, mut) // apply changes to table
	if err != nil {
		panic(err)
		return
	}
	fmt.Printf("Post is saved to BigTable : %s\n", p.Message)
}

func handlerSearch(w http.ResponseWriter, r *http.Request) {
	fmt.Println("Received one request for search")
	lat, _ := strconv.ParseFloat(r.URL.Query().Get("lat"), 64)
	lon, _ := strconv.ParseFloat(r.URL.Query().Get("lon"), 64)

	// range is optional
	ran := DISTANCE
	if val := r.URL.Query().Get("range"); val != "" {
		ran = val + "km"
	}

	fmt.Printf("Search received: %f %f %s\n", lat, lon, ran)

	// Create a client
	client, err := elastic.NewClient(elastic.SetURL(ES_URL), elastic.SetSniff(false))
	if err != nil {
		panic(err)
		return
	}

	// Define geo distance query as specified in
	// https://www.elastic.co/guide/en/elasticsearch/reference/5.2/query-dsl-geo-distance-query.html
	q := elastic.NewGeoDistanceQuery("location")
	q = q.Distance(ran).Lat(lat).Lon(lon)

	// Some delay may range from seconds to minutes. So if you don't get enough results. Try it later.
	searchResult, err := client.Search().
		Index(INDEX).
		Query(q).
		Pretty(true).
		Do()
	if err != nil {
		// Handle error
		panic(err)
	}

	// searchResult is of type SearchResult and returns hits, suggestions,
	// and all kinds of other information from Elasticsearch.
	fmt.Printf("Query took %d milliseconds\n", searchResult.TookInMillis)
	// TotalHits is another convenience function that works even when something goes wrong.
	fmt.Printf("Found a total of %d post\n", searchResult.TotalHits())

	// Each is a convenience function that iterates over hits in a search result.
	// It makes sure you don't need to check for nil values in the response.
	// However, it ignores errors in serialization.
	var typ Post
	var ps []Post
	for _, item := range searchResult.Each(reflect.TypeOf(typ)) { // instance of
		p := item.(Post) // p = (Post) item
		fmt.Printf("Post by %s: %s at lat %v and lon %v\n", p.User, p.Message, p.Location.Lat, p.Location.Lon)
		// Perform filtering based on keywords such as web spam etc.
		if !containsSpam(&p.Message) {
			ps = append(ps, p)
		} else {
			fmt.Printf("Post %s contains spam words, not allowed to display!\n", p.Message)
		}
	}
	js, err := json.Marshal(ps)
	if err != nil {
		panic(err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Write(js)
}

func initSpamWordsSet() []string {
	filteredWords := []string{
		"fuck",
		"shit",
		"bitch",
	}
	return filteredWords
}

func containsSpam(s *string) bool {
	spamSet := initSpamWordsSet()
	for _, spamWord := range spamSet {
		if strings.Contains(*s, spamWord) {
			return true
		}
	}
	return false
}
