package main

import (
	"bytes"
	"crypto/sha1"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

type thread struct {
	Posts []post
}
type post struct {
	No           int
	Resto        int
	Time         int
	Name         string
	Trip         string
	Id           string
	Capcode      string
	Country      string
	Country_name string
	Board_flag   string
	Flag_name    string
	Sub          string
	Com          string
	Tim          int
	Filename     string
	Ext          string
	Fsize        int
	Md5          string
	W            int
	H            int
	Tn_w         int
	Tn_h         int
	Filedeleted  int
	Since4pass   int
	Closed       int
	Archived     int
}

var debug = log.New(os.Stderr, "", log.Ldate|log.Ltime|log.Lshortfile)
var client = http.Client{}

func main() {
	xdg, envOk := os.LookupEnv("XDG_DATA_HOME")
	if !envOk {
		xdg, envOk = os.LookupEnv("HOME")
		xdg = xdg + "/.local/state"
	}
	xdg = xdg + "/aos"

	dataDir := flag.String("data-dir", xdg, "Alternate data directory")
	flag.Parse()
	board := flag.Arg(0)
	threadNo := flag.Arg(1)

	if threadNo == "" {
		if strings.Contains(board, "-") {
			// Special behavior because systemd service can only take one argument
			arr := strings.Split(board, "-")
			board, threadNo = arr[0], arr[1]
		} else {
			fmt.Fprintln(os.Stderr, "Usage:", os.Args[0], "[OPTIONS...] BOARD THREAD")
			fmt.Fprintln(os.Stderr, "options:")
			flag.PrintDefaults()
			os.Exit(1)
		}
	}
	_, err := strconv.Atoi(threadNo)
	if err != nil {
		log.Fatal("'", threadNo, "' is not a valid integer")
	}

	if !envOk && *dataDir == xdg {
		log.Fatal("Couldn't determin a data directory. Set your $XDG_DATA_HOME or $HOME variables, or use --data-dir option")
	}
	if envOk && *dataDir == xdg {
		err = os.MkdirAll(*dataDir, 0755)
		if err != nil {
			debug.Fatal(err)
		}
	}

	_, err = exec.LookPath("ffmpeg")
	if err != nil {
		log.Fatal("Couldn't find command ffmpeg in $PATH")
	}

	db, err := sql.Open("sqlite", *dataDir+"/db.sql")
	if err != nil {
		debug.Fatal(err)
	}
	defer db.Close()

	/*
	 * Not included:
	 ** sticky, closed, now, replies, images, bumplimit, imagelimit, semantic_url, unique_ips, archived, archived_on
	 * Attachment related columns are set at "media" table
	 * board and attachment are extra columns not in the api
	 */
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS post (
			no INTEGER NOT NULL,
			resto INTEGER,
			time INTEGER,
			name TEXT,
			trip TEXT,
			id TEXT,
			capcode TEXT,
			country TEXT,
			country_name TEXT,
			board_flag TEXT,
			flag_name TEXT,
			sub TEXT,
			com TEXT,
			since4pass INTEGER, 
			board TEXT NOT NULL,
			attachment BOOLEAN
		);
	`)
	if err != nil {
		debug.Fatal(err)
	}
	/*
	 * no and board are set to identify linked post
	 * sha is the sha1 hash of the first video stream of the attachment, i.e. it won't change its output based on metadata, container or audio streams
	 * Not included:
	 ** spoiler, custom_spoiler tag, m_img
	 */
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS media (
			no INTEGER NOT NULL,
			board TEXT NOT NULL,
			tim INTEGER,
			filename TEXT,
			ext TEXT,
			fsize INTEGER,
			md5 TEXT,
			w INTEGER,
			h INTEGER,
			tn_w INTEGER,
			tn_h INTEGER,
			filedeleted BOOLEAN,
			sha TEXT
		)
	`)
	if err != nil {
		debug.Fatal(err)
	}

	threadActive := true
	sleeping := false
	greenLight := true

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		for s := range sigChan {
			log.Printf("Recieved signal %s, Shutting down\n", s.String())
			threadActive = false
			greenLight = false
			if sleeping {
				os.Exit(0)
			}
		}
	}()

	for threadActive {
		resp, err := client.Get("https://a.4cdn.org/" + board + "/thread/" + threadNo + ".json")
		if err != nil {
			debug.Fatal(err)
		}
		if resp.StatusCode != 200 {
			log.Fatal("Server replied with status code ", resp.StatusCode)
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			debug.Fatal(err)
		}

		var res thread
		err = json.Unmarshal(body, &res)
		if err != nil {
			debug.Fatal(err)
		}

		threadActive = res.Posts[0].Archived == 0 && res.Posts[0].Closed == 0

		for _, post := range res.Posts {
			if !greenLight {
				break
			}

			row := db.QueryRow("SELECT no FROM post WHERE no == ?", post.No)
			err := row.Scan(nil)
			if err != sql.ErrNoRows {
				// arleady archived
				continue
			}

			_, err = db.Exec(
				"INSERT INTO post VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
				post.No,
				post.Resto,
				post.Time,
				post.Name,
				post.Trip,
				post.Id,
				post.Capcode,
				post.Country,
				post.Country_name,
				post.Board_flag,
				post.Flag_name,
				post.Sub,
				post.Com,
				post.Since4pass,
				board,
				post.Fsize > 0 || post.Filedeleted == 1,
			)
			if err != nil {
				debug.Fatal(err)
			}

			if post.Fsize == 0 && post.Filedeleted != 1 {
				continue
			}
			if post.Filedeleted == 1 {
				log.Printf("File from post no. %d is deleted\n", post.No)
				_, err := db.Exec(
					"INSERT INTO media (no, board, filedeleted) VALUES(?,?,?)",
					post.No,
					board,
					true,
				)
				if err != nil {
					debug.Fatal(err)
				}
				continue
			}

			log.Printf("Downloading %d%s\n", post.Tim, post.Ext)
			resp, err := client.Get("https://i.4cdn.org/" + board + "/" + strconv.Itoa(post.Tim) + post.Ext)
			if err != nil {
				log.Printf("Couldn't get file %d%s: %v\n", post.Tim, post.Ext, err)
				continue
			}
			if resp.StatusCode != 200 {
				log.Printf("Couldn't get file %d%s: Server replied with status code %d", post.Tim, post.Ext, resp.StatusCode)
				continue
			}
			attachment, err := io.ReadAll(resp.Body)
			if err != nil {
				log.Printf("Couldn't get file %d%s: %v\n", post.Tim, post.Ext, err)
				continue
			}

			hash := sha1.New()
			cmd := exec.Command("ffmpeg", "-i", "-", "-map", "0:v:0", "-f", "rawvideo", "-")
			cmd.Stdin = bytes.NewReader(attachment)
			cmd.Stdout = hash
			err = cmd.Run()
			if err != nil {
				debug.Fatal(err)
			}
			videoHash := hash.Sum(nil)

			fileHash, err := base64.StdEncoding.DecodeString(post.Md5)
			if err != nil {
				debug.Fatal(err)
			}

			videoHashStr := hex.EncodeToString(videoHash)
			fileHashStr := hex.EncodeToString(fileHash)
			fileDir := *dataDir + "/media/" + fileHashStr[:2] + "/" + fileHashStr[2:4] + "/" + fileHashStr[4:6]
			filePath := fileDir + "/" + fileHashStr + post.Ext

			err = os.MkdirAll(fileDir, 0755)
			if err != nil {
				debug.Fatal(err)
			}

			_, err = os.Stat(filePath)
			if err != nil {
				downloadThumbnail(fileDir+"/"+fileHashStr+"s.jpg", board, strconv.Itoa(post.Tim)+"s.jpg")
				err := os.WriteFile(filePath, attachment, 0644)
				if err != nil {
					debug.Fatal(err)
				}
			}

			_, err = db.Exec(
				"INSERT INTO media VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)",
				post.No,
				board,
				post.Tim,
				post.Filename,
				post.Ext,
				post.Fsize,
				post.Md5,
				post.W,
				post.H,
				post.Tn_w,
				post.Tn_h,
				post.Filedeleted,
				videoHashStr,
			)
			if err != nil {
				debug.Fatal(err)
			}

			// Adhering to the API rules
			time.Sleep(time.Second * time.Duration(1))
		}

		if threadActive {
			sleeping = true
			time.Sleep(time.Second * time.Duration(300))
			sleeping = false
		}
	}
}

func downloadThumbnail(path, board, id string) {
	// Adhering to the API rules
	time.Sleep(time.Second * time.Duration(1))

	resp, err := client.Get("https://i.4cdn.org/" + board + "/" + id)
	if err != nil {
		log.Printf("Couldn't get file %s: %v\n", id, err)
		return
	}
	if resp.StatusCode != 200 {
		log.Printf("Couldn't get file %s: Server replied with status code %d", id, resp.StatusCode)
		return
	}
	attachment, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("Couldn't get file %s: %v\n", id, err)
		return
	}
	err = os.WriteFile(path, attachment, 0644)
	if err != nil {
		debug.Fatal(err)
	}
}
