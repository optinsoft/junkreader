package main

import (
	"bufio"
	"io/ioutil"
	"log"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"golang.org/x/net/proxy"

	_ "github.com/bdandy/go-socks4"
	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"github.com/go-co-op/gocron"
	"github.com/magisterquis/connectproxy"
	"gopkg.in/yaml.v3"
)

type ProxyInfo struct {
	Type string `yaml:"type"`
	Addr string `yaml:"addr"`
	Auth proxy.Auth
}

type AccountsFileInfo struct {
	Path      string `yaml:"path"`
	Delimiter string `yaml:"delimiter"`
}

type Account struct {
	Username string    `yaml:"username"`
	Password string    `yaml:"password"`
	IMAPAddr string    `yaml:"imapaddr"`
	Proxy    ProxyInfo `yaml:"proxy"`
}

type NotJunkConfig struct {
	From    string `yaml:"from"`
	To      string `yaml:"to"`
	Cc      string `yaml:"cc"`
	Bcc     string `yaml:"bcc"`
	Subject string `yaml:"subject"`
}

type Config struct {
	Cron         string           `yaml:"cron"`
	AccountsFile AccountsFileInfo `yaml:"accountsfile"`
	Accounts     []Account        `yaml:"accounts"`
	NotJunkRules []NotJunkConfig  `yaml:"notjunkrules"`
}

type NotJunkRule struct {
	FromRegExp    *regexp.Regexp
	ToRegExp      *regexp.Regexp
	CcRegExp      *regexp.Regexp
	BccRegExp     *regexp.Regexp
	SubjectRegExp *regexp.Regexp
}

func moveJunkToInbox(c *client.Client, seqset *imap.SeqSet, inboxName string) {
	err := c.Move(seqset, inboxName)
	if err != nil {
		log.Fatalf("Move: %v", err)
	}
}

func isNotJunk(re *regexp.Regexp, addressList []*imap.Address, junk *bool) bool {
	notJunk := false
	for _, address := range addressList {
		if re.MatchString(address.Address()) {
			notJunk = true
			break
		}
	}
	if !notJunk {
		*junk = true
	}
	return notJunk
}

func loadAccountsFromFile(accounts []Account, filePath string, delimiter string) ([]Account, error) {
	readFile, err := os.OpenFile(filePath, os.O_RDONLY, os.ModePerm)
	if err != nil {
		return nil, err
	}
	defer readFile.Close()
	fileScanner := bufio.NewScanner(readFile)
	fileScanner.Split(bufio.ScanLines)
	for fileScanner.Scan() {
		line := fileScanner.Text()
		if len(line) > 0 {
			columns := strings.Split(line, delimiter)
			if len(columns) >= 2 {
				account := Account{Username: columns[0], Password: columns[1]}
				i := 2
				if len(columns) > i {
					if delimiter == ":" && len(columns) > i+1 {
						if len(columns[i]) > 0 {
							account.IMAPAddr = columns[i] + ":" + columns[i+1]
						}
						i += 2
					} else {
						account.IMAPAddr = columns[i]
						i++
					}
				}
				if len(columns) > i {
					if delimiter == ":" && len(columns) > i+1 {
						if len(columns[i]) > 0 {
							account.Proxy.Addr = columns[i] + ":" + columns[i+1]
						}
						i += 2
					} else {
						account.Proxy.Addr = columns[i]
						i++
					}
					if len(account.Proxy.Addr) > 0 {
						if strings.HasPrefix(account.Proxy.Addr, "#") {
							account.Proxy.Addr = account.Proxy.Addr[1:]
							account.Proxy.Type = "https"
						} else if strings.HasPrefix(account.Proxy.Addr, "+") {
							account.Proxy.Addr = account.Proxy.Addr[1:]
							account.Proxy.Type = "socks4"
						} else if strings.HasPrefix(account.Proxy.Addr, "*") {
							account.Proxy.Addr = account.Proxy.Addr[1:]
							account.Proxy.Type = "socks5"
						} else {
							account.Proxy.Type = "https"
						}
					}
				}
				accounts = append(accounts, account)
			}
		}
	}
	return accounts, nil
}

func processConfig(config *Config) error {
	var notJunkRules []*NotJunkRule

	for _, rule := range config.NotJunkRules {
		var r *NotJunkRule = nil
		if len(rule.From) > 0 {
			if r == nil {
				r = new(NotJunkRule)
			}
			re, err := regexp.Compile(rule.From)
			if err != nil {
				log.Printf("Regexp compile: %v", err)
				return err
			}
			r.FromRegExp = re
		}
		if len(rule.To) > 0 {
			if r == nil {
				r = new(NotJunkRule)
			}
			re, err := regexp.Compile(rule.To)
			if err != nil {
				log.Printf("Regexp compile: %v", err)
				return err
			}
			r.ToRegExp = re
		}
		if len(rule.Cc) > 0 {
			if r == nil {
				r = new(NotJunkRule)
			}
			re, err := regexp.Compile(rule.Cc)
			if err != nil {
				log.Printf("Regexp compile: %v", err)
			}
			r.CcRegExp = re
		}
		if len(rule.Bcc) > 0 {
			if r == nil {
				r = new(NotJunkRule)
			}
			re, err := regexp.Compile(rule.Bcc)
			if err != nil {
				log.Printf("Regexp compile: %v", err)
			}
			r.BccRegExp = re
		}
		if len(rule.Subject) > 0 {
			if r == nil {
				r = new(NotJunkRule)
			}
			re, err := regexp.Compile(rule.Subject)
			if err != nil {
				log.Printf("Regexp compile: %v", err)
				return err
			}
			r.SubjectRegExp = re
		}
		if r != nil {
			notJunkRules = append(notJunkRules, r)
		}
	}

	type IMAPInfo struct {
		pattern string
		addr    string
	}

	var imapservices = []IMAPInfo{
		{
			pattern: "(?i)@hotmail",
			addr:    "imap-mail.outlook.com:993",
		},
		{
			pattern: "(?i)@yahoo",
			addr:    "imap.mail.yahoo.com:993",
		},
		{
			pattern: "(?i)@gmail",
			addr:    "imap.gmail.com:993",
		},
	}

	accounts := config.Accounts

	if len(config.AccountsFile.Path) > 0 {
		delimiter := config.AccountsFile.Delimiter
		if len(delimiter) == 0 {
			delimiter = ":"
		}
		var err error
		accounts, err = loadAccountsFromFile(accounts, config.AccountsFile.Path, delimiter)
		if err != nil {
			log.Printf("Load accounts from file: %v", err)
			return err
		}
	}

	for _, account := range accounts {
		username := account.Username
		password := account.Password
		imapaddr := account.IMAPAddr
		if len(imapaddr) == 0 {
			for _, imapinfo := range imapservices {
				service, err := regexp.MatchString(imapinfo.pattern, username)
				if err != nil {
					return err
				}
				if service {
					imapaddr = imapinfo.addr
					break
				}
			}
		}
		if len(imapaddr) == 0 {
			log.Printf("No IMAP info for %s", username)
		}
		var dialer proxy.Dialer = proxy.Direct
		switch account.Proxy.Type {
		case "https":
			proxyUrl, err := url.Parse("http://" + account.Proxy.Addr)
			if err != nil {
				log.Printf("HTTPS proxy: %v", err)
				continue
			}
			if len(account.Proxy.Auth.User) > 0 {
				proxyUrl.User = url.UserPassword(account.Proxy.Auth.User, account.Proxy.Auth.Password)
			}
			httpsDialer, err := connectproxy.New(proxyUrl, proxy.Direct)
			if err != nil {
				log.Printf("HTTPS proxy: %v", err)
				continue
			}
			log.Printf("Using HTTPS proxy: %s", account.Proxy.Addr)
			dialer = httpsDialer
		case "socks4":
			proxyUrl, err := url.Parse("socks4://" + account.Proxy.Addr)
			if err != nil {
				log.Printf("SOCKS4 proxy: %v", err)
				continue
			}
			socksDialer, err := proxy.FromURL(proxyUrl, proxy.Direct)
			if err != nil {
				log.Printf("SOCKS4 proxy: %v", err)
				continue
			}
			log.Printf("Using SOCKS4 proxy: %s", account.Proxy.Addr)
			dialer = socksDialer
		case "socks5":
			var auth *proxy.Auth = nil
			if len(account.Proxy.Auth.User) > 0 {
				auth = &account.Proxy.Auth
			}
			socksDialer, err := proxy.SOCKS5("tcp", account.Proxy.Addr, auth, proxy.Direct)
			if err != nil {
				log.Printf("SOCK5 proxy: %v", err)
				continue
			}
			log.Printf("Using SOCKS5 proxy: %s", account.Proxy.Addr)
			dialer = socksDialer
		default:
			if len(account.Proxy.Type) > 0 {
				log.Printf("Unsupported proxy type: %s", account.Proxy.Type)
				continue
			}
		}
		c, err := client.DialWithDialerTLS(dialer, imapaddr, nil)
		if err != nil {
			log.Printf("Connect to IMAP %s: %v", imapaddr, err)
			return err
		}
		log.Printf("Connected to %s", imapaddr)

		defer c.Logout()

		if err := c.Login(username, password); err != nil {
			log.Printf("Login as %s: %v", username, err)
			continue
		}
		log.Printf("Logged in as %s", username)

		mailboxes := make(chan *imap.MailboxInfo, 10)
		done := make(chan error, 1)
		go func() {
			done <- c.List("", "*", mailboxes)
		}()

		inboxName := ""
		junkName := ""

		for m := range mailboxes {
			switch strings.ToUpper(m.Name) {
			case "INBOX":
				inboxName = m.Name
			case "JUNK":
				junkName = m.Name
			}

		}

		if err := <-done; err != nil {
			log.Printf("List error: %v", err)
			continue
		}

		if len(inboxName) == 0 {
			log.Printf("No Inbox folder found")
			continue
		}
		if len(junkName) == 0 {
			log.Printf("No Junk folder found")
			continue
		}

		junkbox, err := c.Select(junkName, false)
		if err != nil {
			log.Printf("Select %s failed: %v", junkName, err)
			continue
		}

		from := uint32(1)
		to := junkbox.Messages

		if to < from {
			log.Printf("Junk folder is empty")
			continue
		}

		seqset := new(imap.SeqSet)
		seqset.AddRange(from, to)

		messages := make(chan *imap.Message, 10)
		done = make(chan error, 1)
		go func() {
			done <- c.Fetch(seqset, []imap.FetchItem{imap.FetchEnvelope}, messages)
		}()

		moveSeqSet := new(imap.SeqSet)

		for msg := range messages {
			log.Println("Junk message:")
			for _, from := range msg.Envelope.From {
				log.Printf(`  From: %s`, from.Address())
			}
			for _, to := range msg.Envelope.To {
				log.Printf(`  To: %s`, to.Address())
			}
			log.Printf(`  Subject: "%s"`, msg.Envelope.Subject)
			skip := true
			for _, r := range notJunkRules {
				junk := false
				notJunk := false
				if r.FromRegExp != nil {
					notJunk = isNotJunk(r.FromRegExp, msg.Envelope.From, &junk)
				}
				if r.ToRegExp != nil {
					notJunk = isNotJunk(r.ToRegExp, msg.Envelope.To, &junk)
				}
				if r.CcRegExp != nil {
					notJunk = isNotJunk(r.CcRegExp, msg.Envelope.Cc, &junk)
				}
				if r.BccRegExp != nil {
					notJunk = isNotJunk(r.BccRegExp, msg.Envelope.Bcc, &junk)
				}
				if r.SubjectRegExp != nil {
					notJunk = r.SubjectRegExp.MatchString(msg.Envelope.Subject)
					if !notJunk {
						junk = true
					}
				}
				if notJunk && !junk {
					skip = false
					break
				}
			}
			if !skip {
				log.Printf("MOVE to INBOX message #%d", msg.SeqNum)
				moveSeqSet.AddNum(msg.SeqNum)
			} else {
				log.Println("SKIP")
			}
		}

		if err := <-done; err != nil {
			log.Printf("Fetch error: %v", err)
			continue
		}

		if !moveSeqSet.Empty() {
			moveJunkToInbox(c, moveSeqSet, inboxName)
		}
	}

	return nil
}

func readConfig(config *Config) error {
	configFile, err := ioutil.ReadFile("junkreader.yml")
	if err == nil {
		err = yaml.Unmarshal(configFile, config)
	}
	return err
}

func cronTask() {
	var config Config

	err := readConfig(&config)
	if err != nil {
		log.Fatalf("Read config: %v", err)
	}

	processConfig(&config)
}

func main() {
	log.Println("JUNK Reader")

	var config Config

	err := readConfig(&config)
	if err != nil {
		log.Fatalf("Read config: %v", err)
	}

	if len(config.Cron) > 0 {
		log.Printf("cron: %s", config.Cron)
		log.Println("Press Ctrl+C to stop")
		s := gocron.NewScheduler(time.UTC)
		s.Cron(config.Cron).Do(cronTask)
		s.StartBlocking()
	} else {
		processConfig(&config)
	}
}
