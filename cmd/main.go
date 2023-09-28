package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/v2"
	"github.com/knadh/listmonk/internal/bounce"
	"github.com/knadh/listmonk/internal/buflog"
	"github.com/knadh/listmonk/internal/captcha"
	"github.com/knadh/listmonk/internal/core"
	"github.com/knadh/listmonk/internal/events"
	"github.com/knadh/listmonk/internal/i18n"
	"github.com/knadh/listmonk/internal/manager"
	"github.com/knadh/listmonk/internal/media"
	"github.com/knadh/listmonk/internal/subimporter"
	"github.com/knadh/listmonk/models"
	"github.com/knadh/paginator"
	"github.com/knadh/stuffbin"
	"github.com/robfig/cron/v3"
	"gopkg.in/gomail.v2"
)

const (
	emailMsgr = "email"
)

type App struct {
	core       *core.Core
	fs         stuffbin.FileSystem
	db         *sqlx.DB
	queries    *models.Queries
	constants  *constants
	manager    *manager.Manager
	importer   *subimporter.Importer
	messengers map[string]manager.Messenger
	media      media.Store
	i18n       *i18n.I18n
	bounce     *bounce.Manager
	paginator  *paginator.Paginator
	captcha    *captcha.Captcha
	events     *events.Events
	notifTpls  *notifTpls
	about      about
	log        *log.Logger
	bufLog     *buflog.BufLog

	chReload     chan os.Signal
	needsRestart bool
	update       *AppUpdate
	sync.Mutex
}

var (
	evStream = events.New()
	bufLog   = buflog.New(5000)
	lo       = log.New(io.MultiWriter(os.Stdout, bufLog, evStream.ErrWriter()), "",
		log.Ldate|log.Ltime|log.Lshortfile)

	ko      = koanf.New(".")
	fs      stuffbin.FileSystem
	db      *sqlx.DB
	queries *models.Queries

	buildString   string
	versionString string

	appDir      string = "."
	frontendDir string = "frontend"
)

func init() {
	initFlags()

	if ko.Bool("version") {
		fmt.Println(buildString)
		os.Exit(0)
	}

	lo.Println(buildString)

	if ko.Bool("new-config") {
		path := ko.Strings("config")[0]
		if err := newConfigFile(path); err != nil {
			lo.Println(err)
			os.Exit(1)
		}
		lo.Printf("generated %s. Edit and run --install", path)
		os.Exit(0)
	}

	initConfigFiles(ko.Strings("config"), ko)

	if err := ko.Load(env.Provider("LISTMONK_", ".", func(s string) string {
		return strings.Replace(strings.ToLower(
			strings.TrimPrefix(s, "LISTMONK_")), "__", ".", -1)
	}), nil); err != nil {
		lo.Fatalf("error loading config from env: %v", err)
	}

	db = initDB()
	fs = initFS(appDir, frontendDir, ko.String("static-dir"), ko.String("i18n-dir"))

	if ko.Bool("install") {
		install(migList[len(migList)-1].version, db, fs, !ko.Bool("yes"), ko.Bool("idempotent"))
		os.Exit(0)
	}

	if ok, err := checkSchema(db); err != nil {
		log.Fatalf("error checking schema in DB: %v", err)
	} else if !ok {
		lo.Fatal("the database does not appear to be set up. Run --install.")
	}

	if ko.Bool("upgrade") {
		upgrade(db, fs, !ko.Bool("yes"))
		os.Exit(0)
	}

	checkUpgrade(db)

	qMap := readQueries(queryFilePath, db, fs)

	if q, ok := qMap["get-settings"]; ok {
		initSettings(q.Query, db, ko)
	}

	queries = prepareQueries(qMap, db, ko)
}

func sendEmailToSubscribers(db *sqlx.DB, subject string, body string) {
	// 1. Query the database to retrieve email addresses of all subscribers.
	var subscribers []string
	err := db.Select(&subscribers, "SELECT email FROM subscribers")
	if err != nil {
		fmt.Println("Error querying subscribers:", err)
		return
	}

	// Email configuration
	from := "saimouli12345678@gmail.com" // Replace with your email address
	password := "hdel pyso zsqb avte"    // Replace with your email password
	smtpServer := "smtp.gmail.com"
	smtpPort := 465

	for _, subscriberEmail := range subscribers {
		// 2. Send an email to each subscriber.
		m := gomail.NewMessage()
		m.SetHeader("From", from)
		m.SetHeader("To", subscriberEmail) // Use the subscriber's email address
		m.SetHeader("Subject", subject)    // Use the provided subject
		m.SetBody("text/html", body)       // Use the provided body

		d := gomail.NewDialer(smtpServer, smtpPort, from, password)

		if err := d.DialAndSend(m); err != nil {
			fmt.Println("Error sending email to", subscriberEmail, ":", err)
		} else {
			fmt.Println("Email sent successfully to", subscriberEmail)
		}
	}
}

// Example usage for sending Monday morning emails.
func sendMondayMorningEmails(db *sqlx.DB) {
	subject := "Monday Moods"
	body := "Have a great start to the week!"
	sendEmailToSubscribers(db, subject, body)
}

func sendTuesdayMorningEmails(db *sqlx.DB) {
	// Implement logic to send Tuesday morning emails here.
}

func sendWednesdayMorningEmails(db *sqlx.DB) {
	// Implement logic to send Wednesday morning emails here.
}

func sendThursdayMorningEmails(db *sqlx.DB) {
	// Implement logic to send Thursday morning emails here.
}

func sendFridayMorningEmails(db *sqlx.DB) {
	// Implement logic to send Friday morning emails here.
}

func sendSaturdayMorningEmails(db *sqlx.DB) {
	// Implement logic to send Saturday morning emails here.
}

func sendSundayMorningEmails(db *sqlx.DB) {
	// Implement logic to send Sunday morning emails here.
}

func main() {
	app := &App{
		fs:         fs,
		db:         db,
		constants:  initConstants(),
		media:      initMediaStore(),
		messengers: make(map[string]manager.Messenger),
		log:        lo,
		bufLog:     bufLog,
		captcha:    initCaptcha(),
		events:     evStream,

		paginator: paginator.New(paginator.Opt{
			DefaultPerPage: 20,
			MaxPerPage:     50,
			NumPageNums:    10,
			PageParam:      "page",
			PerPageParam:   "per_page",
			AllowAll:       true,
		}),
	}

	app.i18n = initI18n(app.constants.Lang, fs)
	cOpt := &core.Opt{
		Constants: core.Constants{
			SendOptinConfirmation: app.constants.SendOptinConfirmation,
		},
		Queries: queries,
		DB:      db,
		I18n:    app.i18n,
		Log:     lo,
	}

	if err := ko.Unmarshal("bounce.actions", &cOpt.Constants.BounceActions); err != nil {
		lo.Fatalf("error unmarshalling bounce config: %v", err)
	}

	app.core = core.New(cOpt, &core.Hooks{
		SendOptinConfirmation: sendOptinConfirmationHook(app),
	})

	app.queries = queries
	app.manager = initCampaignManager(app.queries, app.constants, app)
	app.importer = initImporter(app.queries, db, app)
	app.notifTpls = initNotifTemplates("/email-templates/*.html", fs, app.i18n, app.constants)
	initTxTemplates(app.manager, app)

	if ko.Bool("bounce.enabled") {
		app.bounce = initBounceManager(app)
		go app.bounce.Run()
	}

	app.messengers[emailMsgr] = initSMTPMessenger(app.manager)

	for _, m := range initPostbackMessengers(app.manager) {
		app.messengers[m.Name()] = m
	}

	for _, m := range app.messengers {
		app.manager.AddMessenger(m)
	}

	app.about = initAbout(queries, db)
	go app.manager.Run()

	fmt.Println("I am here")
	cron := cron.New()

	// Add a cron job to run sendEmailToSubscribers every minute.
	_, err := cron.AddFunc("@every 1m", func() {
		sendEmailToSubscribers(app.db, "subject", "body") // Use app.db instead of db
	})
	if err != nil {
		fmt.Println("Error adding cron job:", err)
		return
	}

	// Schedule a function to stop the cron job after 2 minutes.
	time.AfterFunc(1*time.Minute, func() {
		cron.Stop()
		fmt.Println("Cron job stopped after 1 minutes.")
	})

	// Schedule functions for sending emails on specific days of the week.
	_, err = cron.AddFunc("0 0 * * MON", func() {
		sendMondayMorningEmails(app.db)
	})
	if err != nil {
		fmt.Println("Error adding Monday cron job:", err)
	}

	_, err = cron.AddFunc("0 0 * * TUE", func() {
		sendTuesdayMorningEmails(app.db)
	})
	if err != nil {
		fmt.Println("Error adding Tuesday cron job:", err)
	}

	_, err = cron.AddFunc("0 0 * * WED", func() {
		sendWednesdayMorningEmails(app.db)
	})
	if err != nil {
		fmt.Println("Error adding Wednesday cron job:", err)
	}

	_, err = cron.AddFunc("0 0 * * THU", func() {
		sendThursdayMorningEmails(app.db)
	})
	if err != nil {
		fmt.Println("Error adding Thursday cron job:", err)
	}

	_, err = cron.AddFunc("0 0 * * FRI", func() {
		sendFridayMorningEmails(app.db)
	})
	if err != nil {
		fmt.Println("Error adding Friday cron job:", err)
	}

	_, err = cron.AddFunc("0 0 * * SAT", func() {
		sendSaturdayMorningEmails(app.db)
	})
	if err != nil {
		fmt.Println("Error adding Saturday cron job:", err)
	}

	_, err = cron.AddFunc("0 0 * * SUN", func() {
		sendSundayMorningEmails(app.db)
	})
	if err != nil {
		fmt.Println("Error adding Sunday cron job:", err)
	}

	// Start the cron scheduler.
	cron.Start()

	// Start the app server.
	srv := initHTTPServer(app)

	if ko.Bool("app.check_updates") {
		go checkUpdates(versionString, time.Hour*24, app)
	}

	app.chReload = make(chan os.Signal)
	signal.Notify(app.chReload, syscall.SIGHUP)

	closerWait := make(chan bool)
	<-awaitReload(app.chReload, closerWait, func() {
		cron.Stop()
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
		app.manager.Close()
		app.db.DB.Close()
		for _, m := range app.messengers {
			m.Close()
		}
		closerWait <- true
	})
}
