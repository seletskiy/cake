package main

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/docopt/docopt-go"
)

const usage = `cake - confluence schedule table reader.

Program will read confluence pages which contains schedule in specific format
and grant JSON and TEXT access to them.

Schedule must be presented as single page, which should consist of two kinds of
tables.

First table is describing man-on-duty row-by-row and should look like this:

    +--------+--------------------------------------------+
    | <name> | email@example.com / @link.to.slack.contact |
    +--------+--------------------------------------------+

    E-mail and Slack Contact is optional.

    Each row should be colored in unique way. This color will be used in
    following tables.

First table should be followed by one or more sections, which begins with
header in format:

    <Month>, <Year>

Header must be followed by table, which represents calendar for specified
month:

    +---+---+---+---+---+---+---+
    |Mon|Tue|Wed|Thu|Fri|Sat|Sun|
    +---+---+---+---+---+---+---+
    |   |  1|  2|  3|  4|  5|  6|
    +---+---+---+---+---+---+---+
    |                       ... |
    +---+---+---+---+---+---+---+
    | 27| 28| 29| 30| 31|   |   |
    +---+---+---+---+---+---+---+

    Header is not mandatory.

    Each cell shuld be colored in the same way as rows in the first table.

Usage:
    $0 -h | --help
    $0 [options] --url= --login= --password= -D [--listen=]
    $0 [options] --url= --login= --password= -L [-jc]

Options:
    -h --help              Show this help.
    -L                     List mode.
      -j                   Dump in JSON.
      -c                   Prints only current man on duty.
    -D                     Run in daemon mode and serve schedules by HTTP.
      --listen=<address>   Listen address and port for daemon mode.
                           [default: :8080]
    --url=<url>            Confluence URL to get data from. See more about
                           format above.
    --login=<login>        Confluence user login.
    --password=<password>  Confluence user password.
`

type Duty struct {
	Month string
	Day   int
}

type Master struct {
	Current    bool
	Today      Duty
	Name       string
	Email      string
	Slack      string
	SlackShort string
	Colour     string
	Duty       []Duty
}

func main() {
	args, err := docopt.Parse(
		strings.Replace(usage, "$0", os.Args[0], -1),
		nil, true, "cake 1.0", false,
	)
	if err != nil {
		panic(err)
	}

	confluencePage, err := getConfluencePage(
		args["--url"].(string),
		args["--login"].(string),
		args["--password"].(string),
	)

	if err != nil {
		panic(err)
	}

	var masters []Master

	switch {
	case args["-D"].(bool):
		http.HandleFunc(
			"/",
			func(writer http.ResponseWriter, request *http.Request) {
				masters, err = parseMatersSchedule(confluencePage)
				if err != nil {
					log.Print(err)
				}

				jsonMasters, err := json.Marshal(masters)
				if err != nil {
					log.Print(err)
				}

				writer.Write(jsonMasters)
			},
		)

		log.Printf("starting server at %s", args["--listen"].(string))
		http.ListenAndServe(args["--listen"].(string), nil)

	default:
		masters, err = parseMatersSchedule(confluencePage)
		if err != nil {
			panic(err)
		}
	}

	if args["-c"].(bool) {
		currentMaster := Master{}
		for _, master := range masters {
			if master.Current {
				currentMaster = master
				break
			}
		}

		masters = []Master{currentMaster}
	}

	switch {
	case args["-j"].(bool):
		var jsonMasters []byte
		var err error

		if args["-c"].(bool) {
			jsonMasters, err = json.Marshal(masters[0])
		} else {
			jsonMasters, err = json.Marshal(masters)
		}

		if err != nil {
			panic(err)
		}

		os.Stdout.Write(jsonMasters)

	default:
		tabWriter := tabwriter.NewWriter(os.Stdout, 1, 4, 2, ' ', 0)

		printDutyTable(masters, tabWriter)

		tabWriter.Flush()
	}
}

func getConfluencePage(url, login, password string) (string, error) {
	confluenceRequest, err := http.NewRequest("GET", url, nil)
	if err != nil {
		panic(err)
	}

	confluenceRequest.SetBasicAuth(login, password)

	confluenceResponse, err := http.DefaultClient.Do(confluenceRequest)
	if err != nil {
		panic(err)
	}

	articleBodyRaw, err := ioutil.ReadAll(confluenceResponse.Body)
	if err != nil {
		panic(err)
	}

	article := struct {
		Body struct {
			Storage struct {
				Value string
			}
		}
	}{}

	err = json.Unmarshal(articleBodyRaw, &article)
	if err != nil {
		panic(err)
	}

	return article.Body.Storage.Value, nil
}

func parseMatersSchedule(confluencePage string) ([]Master, error) {
	const (
		parserStateBegin = iota
		parserStateContacts
		parserStateName
		parserStateContactInfo
		parserStateSchedule
		parserStateDay
	)

	var (
		reTagDelimiter = regexp.MustCompile(`(>)|(<)`)
		reContactName  = regexp.MustCompile(
			`<td.*?(rgb\([^)]+\)|highlight-\w+)`,
		)

		reContactEmail = regexp.MustCompile(`[\w.]+@\S+`)
		reContactSlack = regexp.MustCompile(
			`https://.*slack.com/messages/([^"]+)`,
		)
		reContactMonth = regexp.MustCompile(`(\S+), (\d+)`)
	)

	var months = map[string]time.Month{
		"январь":   1,
		"февраль":  2,
		"март":     3,
		"апрель":   4,
		"май":      5,
		"июнь":     6,
		"июль":     7,
		"август":   8,
		"сентябрь": 9,
		"октябрь":  10,
		"ноябрь":   11,
		"декабрь":  12,
	}

	splittedBody := reTagDelimiter.ReplaceAllString(
		confluencePage, "$1\n$2",
	)

	state := parserStateContacts

	masters := []Master{}
	master := &Master{}
	month := ""

	for _, line := range strings.Split(splittedBody, "\n") {
		line = strings.TrimSpace(line)

		switch state {
		case parserStateContacts:
			if line == "</table>" {
				state = parserStateSchedule
			}

			matches := reContactName.FindStringSubmatch(line)

			if len(matches) > 0 {
				master.Colour = matches[1]

				state = parserStateName
			}

		case parserStateName:
			if line == "" {
				continue
			}

			if !strings.ContainsAny(line, "<>") {
				master.Current = false
				master.Name = line

				state = parserStateContactInfo
			}

		case parserStateContactInfo:
			if line == "</tr>" {
				state = parserStateContacts

				masters = append(masters, *master)
			}

			matches := reContactEmail.FindStringSubmatch(line)
			if len(matches) > 0 {
				master.Email = matches[0]
			}

			matches = reContactSlack.FindStringSubmatch(line)
			if len(matches) > 0 {
				master.Slack = matches[0]
				master.SlackShort = matches[1]
			}

		case parserStateSchedule:
			matches := reContactMonth.FindStringSubmatch(line)
			if len(matches) > 0 {
				month = matches[1]
			}

			for i, possibleMaster := range masters {
				if strings.Contains(line, possibleMaster.Colour) {
					master = &masters[i]

					state = parserStateDay
				}
			}

		case parserStateDay:
			day, _ := strconv.Atoi(line)

			if time.Now().Day() == day {
				if months[strings.ToLower(month)] == time.Now().Month() {
					master.Current = true
					master.Today = Duty{
						Month: month,
						Day:   day,
					}
				}
			}

			master.Duty = append(master.Duty, Duty{
				Month: month,
				Day:   day,
			})

			state = parserStateSchedule
		}

	}

	return masters, nil
}

func printDutyTable(masters []Master, writer io.Writer) {
	for _, master := range masters {
		currentFlag := ""
		if master.Current {
			currentFlag = "*"
		}

		writer.Write(
			[]byte(fmt.Sprintf(
				"%-2s%s\t%s\t%s\n",
				currentFlag,
				master.Name, master.Email, master.SlackShort,
			)),
		)

		for _, dutyDate := range master.Duty {
			writer.Write([]byte(
				fmt.Sprintf("    %-2d %s\t\t\n", dutyDate.Day, dutyDate.Month),
			))
		}
	}
}
