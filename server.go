package pivot

//go:generate esc -o static.go -pkg pivot -modtime 1500000000 -prefix ui ui

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/ghetzel/diecast"
	"github.com/ghetzel/go-stockutil/httputil"
	"github.com/ghetzel/go-stockutil/log"
	"github.com/ghetzel/go-stockutil/maputil"
	"github.com/ghetzel/go-stockutil/pathutil"
	"github.com/ghetzel/go-stockutil/stringutil"
	"github.com/ghetzel/go-stockutil/typeutil"
	"github.com/husobee/vestigo"
	"github.com/sniperkit/pivot/backends"
	"github.com/sniperkit/pivot/dal"
	"github.com/sniperkit/pivot/filter"
	"github.com/sniperkit/pivot/util"
	"github.com/urfave/negroni"
)

var DefaultAddress = `127.0.0.1`
var DefaultPort = 29029
var DefaultResultLimit = 25
var DefaultUiDirectory = `embedded`

type Server struct {
	Address          string
	ConnectionString string
	ConnectOptions   backends.ConnectOptions
	UiDirectory      string
	backend          backends.Backend
	endpoints        []util.Endpoint
	routeMap         map[string]util.EndpointResponseFunc
	schemaDefs       []string
}

func NewServer(connectionString ...string) *Server {
	return &Server{
		Address:          fmt.Sprintf("%s:%d", DefaultAddress, DefaultPort),
		ConnectionString: connectionString[0],
		UiDirectory:      DefaultUiDirectory,
		endpoints:        make([]util.Endpoint, 0),
		routeMap:         make(map[string]util.EndpointResponseFunc),
	}
}

func (self *Server) AddSchemaDefinition(filename string) {
	if pathutil.DirExists(filename) {
		if entries, err := ioutil.ReadDir(filename); err == nil {
			for _, entry := range entries {
				if entry.Mode().IsRegular() {
					self.schemaDefs = append(self.schemaDefs, path.Join(filename, entry.Name()))
				}
			}
		}
	} else if pathutil.FileExists(filename) {
		self.schemaDefs = append(self.schemaDefs, filename)
	}
}

func (self *Server) ListenAndServe() error {
	uiDir := self.UiDirectory

	if self.UiDirectory == `embedded` {
		uiDir = `/`
	}

	if backend, err := NewDatabaseWithOptions(self.ConnectionString, self.ConnectOptions); err == nil {
		self.backend = backend
	} else {
		return err
	}

	// if specified, pre-load schema definitions
	for _, filename := range self.schemaDefs {
		if collections, err := LoadSchemataFromFile(filename); err == nil {
			log.Infof("Loaded %d definitions from %v", len(collections), filename)

			for _, collection := range collections {
				self.backend.RegisterCollection(collection)
			}
		} else {
			return err
		}
	}

	server := negroni.New()
	mux := http.NewServeMux()
	router := vestigo.NewRouter()
	ui := diecast.NewServer(uiDir, `*.html`)

	// tell diecast where loopback requests should go
	if strings.HasPrefix(self.Address, `:`) {
		ui.BindingPrefix = fmt.Sprintf("http://localhost%s", self.Address)
	} else {
		ui.BindingPrefix = fmt.Sprintf("http://%s", self.Address)
	}

	if self.UiDirectory == `embedded` {
		ui.SetFileSystem(FS(false))
	}

	if err := ui.Initialize(); err != nil {
		return err
	}

	if err := self.setupRoutes(router); err != nil {
		return err
	}

	mux.Handle(`/api/`, router)
	mux.Handle(`/`, ui)

	server.UseHandler(mux)
	// server.Use(httputil.NewRequestLogger())
	server.Run(self.Address)
	return nil
}

func (self *Server) setupRoutes(router *vestigo.Router) error {
	router.SetGlobalCors(&vestigo.CorsAccessControl{
		AllowOrigin:      []string{"*"},
		AllowCredentials: true,
		AllowMethods:     []string{`GET`, `POST`, `PUT`, `DELETE`},
		MaxAge:           3600 * time.Second,
		AllowHeaders:     []string{"*"},
	})

	router.Get(`/api/status`,
		func(w http.ResponseWriter, req *http.Request) {
			status := map[string]interface{}{
				`backend`: self.backend.GetConnectionString().String(),
			}

			if indexer := self.backend.WithSearch(nil, nil); indexer != nil {
				status[`indexer`] = indexer.IndexConnectionString().String()
			}

			httputil.RespondJSON(w, status)
		})

	router.Get(`/api/collections/:collection`,
		func(w http.ResponseWriter, req *http.Request) {
			name := vestigo.Param(req, `collection`)

			if collection, err := self.backend.GetCollection(name); err == nil {
				collection = injectRequestParamsIntoCollection(req, collection)

				httputil.RespondJSON(w, collection)
			} else {
				httputil.RespondJSON(w, err, http.StatusNotFound)
			}
		})

	queryHandler := func(w http.ResponseWriter, req *http.Request) {
		var query interface{}
		var name string
		var leftField string
		var rightName string
		var rightField string

		collections := strings.Split(vestigo.Param(req, `collection`), `:`)

		switch len(collections) {
		case 1:
			name = collections[0]
		case 2:
			name, leftField = stringutil.SplitPair(collections[0], `.`)
			rightName, rightField = stringutil.SplitPair(collections[1], `.`)
		default:
			httputil.RespondJSON(w, fmt.Errorf("Only two (2) joined collections are supported"), http.StatusBadRequest)
			return
		}

		switch req.Method {
		case `GET`:
			if q := vestigo.Param(req, `_name`); q != `` {
				query = q
			}

		case `POST`:
			fMap := make(map[string]interface{})

			if err := httputil.ParseRequest(req, &fMap); err == nil {
				query = fMap
			} else {
				httputil.RespondJSON(w, err, http.StatusBadRequest)
				return
			}
		}

		if f, err := filterFromRequest(req, query, int64(DefaultResultLimit)); err == nil {
			if collection, err := self.backend.GetCollection(name); err == nil {
				collection = injectRequestParamsIntoCollection(req, collection)

				var queryInterface backends.Indexer

				if search := self.backend.WithSearch(collection); search != nil {
					if rightName == `` {
						queryInterface = search
					} else {
						if rightCollection, err := self.backend.GetCollection(rightName); err == nil {
							// leaving this here, though a little redundant, for when we support heterogeneous backends
							if rightSearch := self.backend.WithSearch(rightCollection); rightSearch != nil {
								queryInterface = backends.NewMetaIndex(
									search,
									collection,
									leftField,
									rightSearch,
									rightCollection,
									rightField,
								)
							} else {
								httputil.RespondJSON(w, fmt.Errorf("Backend %T does not support complex queries.", self.backend), http.StatusBadRequest)
								return
							}
						} else {
							httputil.RespondJSON(w, fmt.Errorf("right-side: %v", err))
							return
						}
					}

					if recordset, err := queryInterface.Query(collection, f); err == nil {
						httputil.RespondJSON(w, recordset)
					} else {
						httputil.RespondJSON(w, err)
					}
				} else {
					httputil.RespondJSON(w, fmt.Errorf("Backend %T does not support complex queries.", self.backend), http.StatusBadRequest)
				}
			} else if dal.IsCollectionNotFoundErr(err) {
				httputil.RespondJSON(w, err, http.StatusNotFound)
			} else {
				httputil.RespondJSON(w, err)
			}
		} else {
			httputil.RespondJSON(w, err, http.StatusBadRequest)
		}
	}

	router.Post(`/api/collections/:collection/query/`, queryHandler)
	router.Get(`/api/collections/:collection/query/`, queryHandler)
	router.Get(`/api/collections/:collection/where/*urlquery`, queryHandler)

	router.Get(`/api/collections/:collection/aggregate/:fields`,
		func(w http.ResponseWriter, req *http.Request) {
			name := vestigo.Param(req, `collection`)
			fields := strings.Split(vestigo.Param(req, `fields`), `,`)
			aggregations := strings.Split(httputil.Q(req, `fn`, `count`), `,`)

			if f, err := filterFromRequest(req, httputil.Q(req, `q`, `all`), 0); err == nil {
				if collection, err := self.backend.GetCollection(name); err == nil {
					collection = injectRequestParamsIntoCollection(req, collection)

					if aggregator := self.backend.WithAggregator(collection); aggregator != nil {
						results := make(map[string]interface{})

						for _, field := range fields {
							fieldResults := make(map[string]interface{})

							for _, aggregation := range aggregations {
								var value interface{}
								var err error

								switch aggregation {
								case `count`:
									value, err = aggregator.Count(collection, f)
								case `sum`:
									value, err = aggregator.Sum(collection, field, f)
								case `min`:
									value, err = aggregator.Minimum(collection, field, f)
								case `max`:
									value, err = aggregator.Maximum(collection, field, f)
								case `avg`:
									value, err = aggregator.Average(collection, field, f)
								default:
									httputil.RespondJSON(w, fmt.Errorf("Unsupported aggregator '%s'", aggregation), http.StatusBadRequest)
									return
								}

								if err != nil {
									httputil.RespondJSON(w, err)
									return
								}

								fieldResults[aggregation] = value
							}

							results[field] = fieldResults
						}

						httputil.RespondJSON(w, results)
					} else {
						httputil.RespondJSON(w, fmt.Errorf("Backend %T does not support aggregations.", self.backend), http.StatusBadRequest)
					}
				} else if dal.IsCollectionNotFoundErr(err) {
					httputil.RespondJSON(w, err, http.StatusNotFound)
				} else {
					httputil.RespondJSON(w, err)
				}
			} else {
				httputil.RespondJSON(w, err, http.StatusBadRequest)
			}
		})

	router.Get(`/api/collections/:collection/list/*fields`,
		func(w http.ResponseWriter, req *http.Request) {
			name := vestigo.Param(req, `collection`)
			fieldNames := vestigo.Param(req, `_name`)

			if f, err := filterFromRequest(req, httputil.Q(req, `q`, `all`), 0); err == nil {
				if collection, err := self.backend.GetCollection(name); err == nil {
					collection = injectRequestParamsIntoCollection(req, collection)

					if search := self.backend.WithSearch(collection); search != nil {
						fields := strings.TrimPrefix(fieldNames, `/`)

						if recordset, err := search.ListValues(collection, strings.Split(fields, `/`), f); err == nil {
							httputil.RespondJSON(w, recordset)
						} else {
							httputil.RespondJSON(w, err)
						}
					} else {
						httputil.RespondJSON(w, fmt.Errorf("Backend %T does not support complex queries.", self.backend), http.StatusBadRequest)
					}
				} else if dal.IsCollectionNotFoundErr(err) {
					httputil.RespondJSON(w, err, http.StatusNotFound)
				} else {
					httputil.RespondJSON(w, err)
				}
			} else {
				httputil.RespondJSON(w, err, http.StatusBadRequest)
			}
		})

	router.Delete(`/api/collections/:collection/where/*urlquery`,
		func(w http.ResponseWriter, req *http.Request) {
			name := vestigo.Param(req, `collection`)
			query := vestigo.Param(req, `_name`)

			if f, err := filter.Parse(query); err == nil {
				if err := self.backend.Delete(name, f); err == nil {
					httputil.RespondJSON(w, nil)
				} else {
					httputil.RespondJSON(w, err, http.StatusBadRequest)
				}
			} else {
				httputil.RespondJSON(w, err, http.StatusBadRequest)
			}
		})

	router.Get(`/api/collections/:collection/records/:id`,
		func(w http.ResponseWriter, req *http.Request) {
			var id interface{}
			var fields []string
			name := vestigo.Param(req, `collection`)

			if ids := strings.Split(vestigo.Param(req, `id`), `:`); len(ids) == 1 {
				id = ids[0]
			} else {
				id = ids
			}

			if v := httputil.Q(req, `fields`); v != `` {
				fields = strings.Split(v, `,`)
			}

			if record, err := self.backend.Retrieve(name, id, fields...); err == nil {
				httputil.RespondJSON(w, record)
			} else if strings.HasSuffix(err.Error(), `does not exist`) {
				httputil.RespondJSON(w, err, http.StatusNotFound)
			} else {
				httputil.RespondJSON(w, err)
			}
		})

	router.Post(`/api/collections/:collection/records/:id`,
		func(w http.ResponseWriter, req *http.Request) {
			var record dal.Record

			if err := httputil.ParseRequest(req, &record); err == nil {
				recordset := dal.NewRecordSet(&record)
				name := vestigo.Param(req, `collection`)
				var err error

				if self.backend.Exists(name, record.ID) {
					err = self.backend.Update(name, recordset)
				} else {
					err = self.backend.Insert(name, recordset)
				}

				if err == nil {
					httputil.RespondJSON(w, &record)
				} else {
					httputil.RespondJSON(w, err)
				}
			} else {
				httputil.RespondJSON(w, err, http.StatusBadRequest)
			}
		})

	router.Post(`/api/collections/:collection/records`,
		func(w http.ResponseWriter, req *http.Request) {
			var recordset dal.RecordSet

			if err := httputil.ParseRequest(req, &recordset); err == nil {
				name := vestigo.Param(req, `collection`)

				if err := self.backend.Insert(name, &recordset); err == nil {
					httputil.RespondJSON(w, &recordset)
				} else {
					httputil.RespondJSON(w, err)
				}
			} else {
				httputil.RespondJSON(w, err, http.StatusBadRequest)
			}
		})

	router.Put(`/api/collections/:collection/records`,
		func(w http.ResponseWriter, req *http.Request) {
			var recordset dal.RecordSet

			if err := httputil.ParseRequest(req, &recordset); err == nil {
				name := vestigo.Param(req, `collection`)

				if err := self.backend.Update(name, &recordset); err == nil {
					httputil.RespondJSON(w, nil)
				} else {
					httputil.RespondJSON(w, err)
				}
			} else {
				httputil.RespondJSON(w, err, http.StatusBadRequest)
			}
		})

	router.Delete(`/api/collections/:collection/records/*id`,
		func(w http.ResponseWriter, req *http.Request) {
			var id interface{}
			name := vestigo.Param(req, `collection`)

			if ids := strings.Split(vestigo.Param(req, `_name`), `/`); len(ids) == 1 {
				id = ids[0]
			} else {
				id = ids
			}

			if err := self.backend.Delete(name, id); err == nil {
				httputil.RespondJSON(w, nil)
			} else {
				httputil.RespondJSON(w, err)
			}
		})

	router.Post(`/api/collections/:collection`,
		func(w http.ResponseWriter, req *http.Request) {
			var recordset dal.RecordSet
			name := vestigo.Param(req, `collection`)

			if err := json.NewDecoder(req.Body).Decode(&recordset); err == nil {
				if err := self.backend.Insert(name, &recordset); err == nil {
					httputil.RespondJSON(w, nil)
				} else {
					httputil.RespondJSON(w, err)
				}
			} else {
				httputil.RespondJSON(w, err, http.StatusBadRequest)
			}
		})

	router.Put(`/api/collections/:collection`,
		func(w http.ResponseWriter, req *http.Request) {
			var recordset dal.RecordSet
			name := vestigo.Param(req, `collection`)

			if err := json.NewDecoder(req.Body).Decode(&recordset); err == nil {
				if err := self.backend.Update(name, &recordset); err == nil {
					httputil.RespondJSON(w, nil)
				} else {
					httputil.RespondJSON(w, err)
				}
			} else {
				httputil.RespondJSON(w, err, http.StatusBadRequest)
			}
		})

	router.Get(`/api/schema`,
		func(w http.ResponseWriter, req *http.Request) {
			if names, err := self.backend.ListCollections(); err == nil {
				httputil.RespondJSON(w, names)
			} else {
				httputil.RespondJSON(w, err)
			}
		})

	router.Post(`/api/schema`,
		func(w http.ResponseWriter, req *http.Request) {
			var collections []dal.Collection

			if body, err := ioutil.ReadAll(req.Body); err == nil {
				var collection dal.Collection

				if err := json.Unmarshal(body, &collection); err == nil {
					collections = append(collections, collection)
				} else if strings.Contains(err.Error(), `cannot unmarshal array `) {
					if err := json.Unmarshal(body, &collections); err != nil {
						httputil.RespondJSON(w, err, http.StatusBadRequest)
						return
					}
				} else {
					httputil.RespondJSON(w, err, http.StatusBadRequest)
				}
			} else {
				httputil.RespondJSON(w, err, http.StatusBadRequest)
				return
			}

			var errors []error

			for _, collection := range collections {
				if err := self.backend.CreateCollection(&collection); err == nil {
					httputil.RespondJSON(w, collection, http.StatusCreated)

				} else if len(collections) == 1 {
					if dal.IsExistError(err) {
						httputil.RespondJSON(w, err, http.StatusConflict)
					} else {
						httputil.RespondJSON(w, err)
					}

					return
				} else {
					errors = append(errors, err)
				}
			}

			if len(errors) > 0 {
				httputil.RespondJSON(w, errors, http.StatusBadRequest)
			}
		})

	router.Get(`/api/schema/:collection`,
		func(w http.ResponseWriter, req *http.Request) {
			name := vestigo.Param(req, `collection`)

			if collection, err := self.backend.GetCollection(name); err == nil {
				collection = injectRequestParamsIntoCollection(req, collection)

				httputil.RespondJSON(w, collection)
			} else {
				httputil.RespondJSON(w, err, http.StatusBadRequest)
			}
		})

	router.Delete(`/api/schema/:collection`,
		func(w http.ResponseWriter, req *http.Request) {
			name := vestigo.Param(req, `collection`)

			if err := self.backend.DeleteCollection(name); err == nil {
				httputil.RespondJSON(w, nil)
			} else {
				httputil.RespondJSON(w, err, http.StatusBadRequest)
			}
		})

	return nil
}

func injectRequestParamsIntoCollection(req *http.Request, collection *dal.Collection) *dal.Collection {
	// shallow copy the collection so we can screw with it
	c := *collection
	collection = &c

	if v := httputil.Q(req, `index`); v != `` {
		collection.IndexName = v
	}

	if v := httputil.Q(req, `keys`); v != `` {
		collection.IndexCompoundFields = strings.Split(v, `,`)
	}

	if v := httputil.Q(req, `joiner`); v != `` {
		collection.IndexCompoundFieldJoiner = v
	}

	return collection
}

func filterFromRequest(req *http.Request, filterIn interface{}, defaultLimit int64) (*filter.Filter, error) {
	limit := int(httputil.QInt(req, `limit`, defaultLimit))
	offset := int(httputil.QInt(req, `offset`))
	var f *filter.Filter

	switch filterIn.(type) {
	case string:
		if flt, err := filter.Parse(filterIn.(string)); err == nil {
			f = flt
		} else {
			return nil, err
		}
	case *filter.Filter:
		f = filterIn.(*filter.Filter)

	default:
		if typeutil.IsMap(filterIn) {
			if fMap, err := maputil.Compact(maputil.Autotype(filterIn)); err == nil {
				if flt, err := filter.FromMap(fMap); err == nil {
					f = flt
				} else {
					return nil, fmt.Errorf("filter parse error: %v", err)
				}
			} else {
				return nil, fmt.Errorf("map error: %v", err)
			}
		} else {
			return nil, fmt.Errorf("Unsupported filter input type %T", filterIn)
		}
	}

	f.Limit = limit
	f.Offset = offset

	if v := httputil.Q(req, `sort`); v != `` {
		f.Sort = strings.Split(v, `,`)
	}

	if v := httputil.Q(req, `fields`); v != `` {
		f.Fields = strings.Split(v, `,`)
	}

	return f, nil
}
