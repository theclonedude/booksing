package main

import (
	"fmt"
	"html/template"
	"io/ioutil"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	"github.com/gnur/booksing"
	"github.com/gnur/booksing/meili"
	"github.com/gnur/booksing/storm"
	"github.com/markbates/goth"
	"github.com/markbates/goth/gothic"
	"github.com/markbates/goth/providers/google"
	"github.com/markbates/pkger"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/crypto/argon2"

	"github.com/kelseyhightower/envconfig"
	log "github.com/sirupsen/logrus"
)

var salt = []byte("kcqbBu5yrpEaZFpGVdR6z4ke2Sr7UtgxhFCxMtEeSECy6zuYDXkV9jfU")

// V is the holder struct for all possible template values
type V struct {
	Results    int
	Error      error
	Books      []booksing.Book
	Book       *booksing.Book
	Users      []booksing.User
	Downloads  []booksing.Download
	Q          string
	TimeTaken  int
	IsAdmin    bool
	Username   string
	TotalBooks int
	Limit      int64
	Offset     int64
	Indexing   bool
}

type configuration struct {
	FQDN      string `default:"http://localhost:7132"`
	AdminUser string `required:"true"`
	BookDir   string `default:"."`
	ImportDir string `default:"./import"`
	FailDir   string `default:"./failed"`
	Database  string `default:"file://booksing.db"`
	Meili     struct {
		Host  string `default:"http://localhost:7700"`
		Index string `default:"books"`
		Key   string `required:"true"`
	}
	LogLevel     string `default:"info"`
	BindAddress  string `default:"localhost:7132"`
	Timezone     string `default:"Europe/Amsterdam"`
	BatchSize    int    `default:"50"`
	Workers      int    `default:"5"`
	SaveInterval string `default:"10s"`
	Secret       []byte `required:"true"`
}

func main() {
	var cfg configuration
	err := envconfig.Process("booksing", &cfg)
	if err != nil {
		log.WithField("err", err).Fatal("Could not parse full config from environment")
	}

	logLevel, err := log.ParseLevel(cfg.LogLevel)
	if err == nil {
		log.SetLevel(logLevel)
	}
	if cfg.ImportDir == "" {
		cfg.ImportDir = path.Join(cfg.BookDir, "import")
	}

	var db database
	if strings.HasPrefix(cfg.Database, "file://") {
		log.WithField("filedbpath", cfg.Database).Debug("using this file")
		db, err = storm.New(strings.TrimPrefix(cfg.Database, "file://"))
		if err != nil {
			log.WithField("err", err).Fatal("could not create fileDB")
		}
		defer db.Close()
	} else {
		log.Fatal("invalid database chosen")
	}

	interval, err := time.ParseDuration(cfg.SaveInterval)
	if err != nil {
		interval = 10 * time.Second
	}

	tz, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		log.WithField("err", err).Fatal("could not load timezone")
	}

	var s search
	s, err = meili.New(cfg.Meili.Host, cfg.Meili.Index, cfg.Meili.Key)
	if err != nil {
		log.WithField("err", err).Fatal("unable to start meili client")
	}

	tpl := template.New("")
	tpl.Funcs(templateFunctions)

	goth.UseProviders(
		google.New(os.Getenv("GOOGLE_KEY"), os.Getenv("GOOGLE_SECRET"), cfg.FQDN+"/auth/google/callback"),
	)
	gothic.GetProviderName = func(req *http.Request) (string, error) {
		return "google", nil
	}

	err = pkger.Walk("/cmd/ui/templates", func(path string, info os.FileInfo, err error) error {
		if strings.HasSuffix(path, ".html") {
			log.WithField("path", path).Debug("loading template")
			f, err := pkger.Open(path)
			if err != nil {
				return err
			}
			sl, err := ioutil.ReadAll(f)
			if err != nil {
				return err
			}
			_, err = tpl.Parse(string(sl))
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		log.WithField("err", err).Fatal("could not read templates")
		return
	}

	app := booksingApp{
		db:           db,
		s:            s,
		bookDir:      cfg.BookDir,
		importDir:    cfg.ImportDir,
		timezone:     tz,
		adminUser:    cfg.AdminUser,
		logger:       log.WithField("app", "booksing"),
		cfg:          cfg,
		bookQ:        make(chan string),
		resultQ:      make(chan parseResult),
		meiliQ:       make(chan booksing.Book),
		saveInterval: interval,
		sessionMap:   sync.Map{},
	}

	if cfg.ImportDir != "" {
		go app.refreshLoop()
		for w := 0; w < 5; w++ { //not sure yet how concurrent-proof my solution is
			go app.bookParser()
		}
		go app.resultParser()
		go app.meiliUpdater()
	}

	r := gin.New()
	key := argon2.IDKey(app.cfg.Secret, salt, 4, 4*1024, 2, 32)
	store := cookie.NewStore(key)
	store.Options(sessions.Options{
		MaxAge:   60 * 60 * 24 * 30, //~30 days
		HttpOnly: true,
		Secure:   strings.HasPrefix(app.cfg.FQDN, "https"),
		SameSite: http.SameSiteLaxMode,
		Path:     "/",
	})
	r.Use(sessions.Sessions("booksing", store))
	r.Use(Logger(app.logger), gin.Recovery())
	r.SetHTMLTemplate(tpl)

	static := r.Group("/", func(c *gin.Context) {
		c.Header("Cache-Control", "public, max-age=86400, immutable")
	})
	static.StaticFS("/static", pkger.Dir("/cmd/ui/static"))

	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	r.GET("/login", func(c *gin.Context) {
		c.HTML(200, "login.html", nil)
	})

	qr := r.Group("/qr")
	{
		qr.GET("/login", func(c *gin.Context) {
			c.HTML(200, "qr-auth.html", gin.H{
				"AuthCode": randID(),
			})
		})

		qr.GET("/img/:code", app.generateQR)

		qr.GET("/poll/:code", func(c *gin.Context) {
			code := c.Param("code")
			user, ok := app.sessionMap.Load(code)
			if !ok {
				if c.Query("method") == "manual" {
					c.Redirect(http.StatusFound, c.Request.Referer())
				} else {
					c.JSON(200, gin.H{
						"status": "no",
					})
				}
				return
			}
			app.sessionMap.Delete("username")
			sess := sessions.Default(c)
			sess.Set("username", user)
			err := sess.Save()
			if err != nil {
				app.logger.WithError(err).Error("failed saving session")
				if c.Query("method") == "manual" {
					c.Redirect(http.StatusFound, c.Request.Referer())
				} else {
					c.JSON(200, gin.H{
						"status": "no",
					})
				}
				return
			}
			if c.Query("method") == "manual" {
				c.Redirect(http.StatusFound, "/")
				return
			}
			c.JSON(200, gin.H{
				"status": "yes",
			})
		})

		qrAuth := qr.Group("", app.BearerTokenMiddleware())
		{
			qrAuth.GET("/authorize/:code", func(c *gin.Context) {
				code := c.Param("code")
				c.HTML(200, "qr-approve.html", gin.H{
					"AuthCode": code,
				})
			})
			qrAuth.GET("/approve/:code", func(c *gin.Context) {
				code := c.Param("code")
				sess := sessions.Default(c)
				app.sessionMap.Store(code, sess.Get("username"))
				c.Redirect(http.StatusFound, "/")
			})
		}
	}

	r.GET("/kill", func(c *gin.Context) {
		app.logger.Fatal("Killing so I get restarted anew")
	})

	r.GET("/status", func(c *gin.Context) {
		c.JSON(200, gin.H{
			"status": app.state,
		})
	})

	r.GET("/auth/google", func(c *gin.Context) {
		if _, err := gothic.CompleteUserAuth(c.Writer, c.Request); err == nil {
			c.Redirect(302, "/")
			return
		}
		gothic.BeginAuthHandler(c.Writer, c.Request)
	})
	r.GET("/auth/google/callback", func(c *gin.Context) {
		u, err := gothic.CompleteUserAuth(c.Writer, c.Request)
		if err != nil {
			c.HTML(200, "error.html", V{
				Error: err,
			})
			return
		}
		sess := sessions.Default(c)
		sess.Set("username", u.Email)
		err = sess.Save()
		app.logger.WithField("username", u.Email).Info("Storing username in session")
		if err != nil {
			app.logger.WithField("err", err).Error("Could not save session")
			c.AbortWithStatus(http.StatusForbidden)
			return
		}
		c.Redirect(302, "/")
	})

	auth := r.Group("/")
	auth.Use(app.BearerTokenMiddleware())
	{
		auth.GET("/", app.search)
		auth.GET("/bookmarks", app.bookmarks)
		auth.GET("/rotateShelve/:hash", app.rotateIcon)
		auth.POST("/rotateShelve/:hash", app.rotateIcon)
		auth.GET("/download", app.downloadBook)
		auth.GET("/icons/:hash", app.serveIcon)

	}

	admin := r.Group("/admin")
	admin.Use(gin.Recovery(), app.BearerTokenMiddleware(), app.mustBeAdmin())
	{
		admin.GET("/users", app.showUsers)
		admin.GET("/downloads", app.showDownloads)
		admin.POST("/delete/:hash", app.deleteBook)
		admin.POST("user/:username", app.updateUser)
		admin.POST("/adduser", app.addUser)
	}

	log.Info("booksing is now running")
	port := os.Getenv("PORT")

	if port == "" {
		port = cfg.BindAddress
	} else {
		port = fmt.Sprintf(":%s", port)
	}

	err = r.Run(port)
	if err != nil {
		log.WithField("err", err).Fatal("unable to start running")
	}
}

func (app *booksingApp) IsUserAdmin(c *gin.Context) bool {

	return true
}