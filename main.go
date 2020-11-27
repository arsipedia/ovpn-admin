package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"gopkg.in/alecthomas/kingpin.v2"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"time"
)

const (
	usernameRegexp    = `^([a-zA-Z0-9_.-])+$`
	downloadCertsApiUrl = "/api/data/certs/download"
	downloadCcdApiUrl = "/api/data/ccd/download"
	certsArchiveFileName = "certs.tar.gz"
	ccdArchiveFileName = "ccd.tar.gz"
	indexTxtDateLayout = "060102150405Z"
	stringDateFormat = "2006-01-02 15:04:05"
	ovpnStatusDateLayout = "Mon Jan 2 15:04:05 2006"
)

var (

	listenHost      		= kingpin.Flag("listen.host","host for openvpn-admin").Default("0.0.0.0").String()
	listenPort      		= kingpin.Flag("listen.port","port for openvpn-admin").Default("8080").String()
    serverRole              = kingpin.Flag("role","server role master or slave").Default("master").HintOptions("master", "slave").String()
	masterHost              = kingpin.Flag("master.host","url for master server").Default("http://127.0.0.1").String()
	masterBasicAuthUser		= kingpin.Flag("master.basic-auth.user","user for basic auth on master server url").Default("").String()
	masterBasicAuthPassword = kingpin.Flag("master.basic-auth.password","password for basic auth on master server url").Default("").String()
	masterSyncFrequency     = kingpin.Flag("master.sync-frequency", "master host data sync frequency in seconds.").Default("600").Int()
	masterSyncToken         = kingpin.Flag("master.sync-token", "master host data sync security token").Default("justasimpleword").PlaceHolder("TOKEN").String()
	openvpnServer      		= kingpin.Flag("ovpn.host","host(s) for openvpn server").Default("127.0.0.1:7777").PlaceHolder("HOST:PORT").Strings()
	openvpnNetwork          = kingpin.Flag("ovpn.network","network for openvpn server").Default("172.16.100.0/24").String()
	mgmtListenHost          = kingpin.Flag("mgmt.host","host for openvpn server mgmt interface").Default("127.0.0.1").String()
	mgmtListenPort          = kingpin.Flag("mgmt.port","port for openvpn server mgmt interface").Default("8989").String()
	metricsPath 			= kingpin.Flag("metrics.path",  "URL path for surfacing collected metrics").Default("/metrics").String()
	easyrsaDirPath     		= kingpin.Flag("easyrsa.path", "path to easyrsa dir").Default("/mnt/easyrsa").String()
	indexTxtPath    		= kingpin.Flag("easyrsa.index-path", "path to easyrsa index file.").Default("/mnt/easyrsa/pki/index.txt").String()
	ccdDir          		= kingpin.Flag("ccd.path", "path to client-config-dir").Default("/mnt/ccd").String()
	staticPath      		= kingpin.Flag("static.path", "path to static dir").Default("./static").String()
	debug           		= kingpin.Flag("debug", "Enable debug mode.").Default("false").Bool()

	certsArchivePath        = "/tmp/" + certsArchiveFileName
	ccdArchivePath          = "/tmp/" + ccdArchiveFileName

)

var (

	ovpnServerCertExpire = prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "ovpn_server_cert_expire",
			Help: "openvpn server certificate expire time in days",
		},
	)

	ovpnServerCaCertExpire = prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "ovpn_server_ca_cert_expire",
			Help: "openvpn server CA certificate expire time in days",
		},
	)

	ovpnClientsTotal = prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "ovpn_clients_total",
			Help: "total openvpn users",
		},
	)

	ovpnClientsRevoked = prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "ovpn_clients_revoked",
			Help: "revoked openvpn users",
		},
	)

	ovpnClientsExpired = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "ovpn_clients_expired",
		Help: "expired openvpn users",
	},
	)

	ovpnClientsConnected = prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "ovpn_clients_connected",
			Help: "connected openvpn users",
		},
	)

	ovpnClientCertificateExpire = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "ovpn_client_cert_expire",
		Help: "openvpn user certificate expire time in days",
	},
		[]string{"client"},
	)

	ovpnClientConnectionInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "ovpn_client_connection_info",
			Help: "openvpn user connection info. ip - assigned address from opvn network. value - last time when connection was refreshed in unix format",
		},
		[]string{"client", "ip"},
	)

	ovpnClientConnectionFrom = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "ovpn_client_connection_from",
		Help: "openvpn user connection info. ip - from which address connection was initialized. value - time when connection was initialized in unix format",
	},
		[]string{"client", "ip"},
	)

	ovpnClientBytesReceived = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "ovpn_client_bytes_received",
			Help: "openvpn user bytes received",
		},
		[]string{"client"},
	)

	ovpnClientBytesSent = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "ovpn_client_bytes_sent",
			Help: "openvpn user bytes sent",
		},
		[]string{"client"},
	)

)

type OpenvpnAdmin struct {
	role string
	lastSyncTime string
	lastSuccessfulSyncTime  string
	masterHostBasicAuth bool
	masterSyncToken string
	clients []OpenvpnClient
	activeClients []clientStatus
	promRegistry *prometheus.Registry
}

type OpenvpnServer struct {
	Host string
	Port  string
}

type openvpnClientConfig struct {
	Hosts []OpenvpnServer
	CA   string
	Cert string
	Key  string
	TLS  string
}

type OpenvpnClient struct {
	Identity            string      `json:"Identity"`
	AccountStatus       string      `json:"AccountStatus"`
    ExpirationDate      string      `json:"ExpirationDate"`
    RevocationDate      string      `json:"RevocationDate"`
	ConnectionStatus    string      `json:"ConnectionStatus"`
}

type ccdRoute struct {
	Address         string      `json:"Address"`
	Mask            string      `json:"Mask"`
	Description     string      `json:"Description"`
}

type Ccd struct {
    User            string      `json:"User"`
    ClientAddress   string      `json:"ClientAddress"`
	CustomRoutes    []ccdRoute  `json:"CustomRoutes"`
}

type indexTxtLine struct {
	Flag              string
	ExpirationDate    string
	RevocationDate    string
	SerialNumber      string
	Filename          string
	DistinguishedName string
	Identity          string
}

type clientStatus struct {
	CommonName             string
	RealAddress            string
	BytesReceived          string
	BytesSent              string
	ConnectedSince         string
	VirtualAddress         string
	LastRef                string
	ConnectedSinceFormatted string
	LastRefFormatted        string
}

func (oAdmin *OpenvpnAdmin) userListHandler(w http.ResponseWriter, r *http.Request) {
	usersList, _ := json.Marshal(oAdmin.clients)
	fmt.Fprintf(w, "%s", usersList)
}

func (oAdmin *OpenvpnAdmin) userStatisticHandler(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	userStatistic, _ := json.Marshal(oAdmin.getUserStatistic(r.FormValue("username")))
	fmt.Fprintf(w, "%s", userStatistic)
}

func (oAdmin *OpenvpnAdmin) userCreateHandler(w http.ResponseWriter, r *http.Request) {
	if oAdmin.role == "slave" {
		http.Error(w, `{"status":"error"}`, http.StatusLocked)
		return
	}
	r.ParseForm()
	userCreated, userCreateStatus := oAdmin.userCreate(r.FormValue("username"))

    if userCreated {
        w.WriteHeader(http.StatusOK)
        fmt.Fprintf(w, userCreateStatus)
        return
    } else {
	    http.Error(w, userCreateStatus, http.StatusUnprocessableEntity)
    }
}

func (oAdmin *OpenvpnAdmin) userRevokeHandler(w http.ResponseWriter, r *http.Request) {
	if oAdmin.role == "slave" {
		http.Error(w, `{"status":"error"}`, http.StatusLocked)
		return
	}
	r.ParseForm()
	fmt.Fprintf(w, "%s", oAdmin.userRevoke(r.FormValue("username")))
}

func (oAdmin *OpenvpnAdmin) userUnrevokeHandler(w http.ResponseWriter, r *http.Request) {
	if oAdmin.role == "slave" {
		http.Error(w, `{"status":"error"}`, http.StatusLocked)
		return
	}

	r.ParseForm()
	fmt.Fprintf(w, "%s", oAdmin.userUnrevoke(r.FormValue("username")))
}

func (oAdmin *OpenvpnAdmin) userShowConfigHandler(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	fmt.Fprintf(w, "%s", oAdmin.renderClientConfig(r.FormValue("username")))
}

func (oAdmin *OpenvpnAdmin) userDisconnectHandler(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
// 	fmt.Fprintf(w, "%s", userDisconnect(r.FormValue("username")))
	fmt.Fprintf(w, "%s", r.FormValue("username"))
}

func (oAdmin *OpenvpnAdmin) userShowCcdHandler(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	ccd, _ := json.Marshal(oAdmin.getCcd(r.FormValue("username")))
	fmt.Fprintf(w, "%s", ccd)
}

func (oAdmin *OpenvpnAdmin) userApplyCcdHandler(w http.ResponseWriter, r *http.Request) {
	if oAdmin.role == "slave" {
		http.Error(w, `{"status":"error"}`, http.StatusLocked)
		return
	}
    var ccd Ccd
    if r.Body == nil {
        http.Error(w, "Please send a request body", http.StatusBadRequest)
        return
    }

    err := json.NewDecoder(r.Body).Decode(&ccd)
    if err != nil {
        log.Println(err)
    }

    ccdApplied, applyStatus := oAdmin.modifyCcd(ccd)

    if ccdApplied {
        w.WriteHeader(http.StatusOK)
        fmt.Fprintf(w, applyStatus)
        return
    } else {
	    http.Error(w, applyStatus, http.StatusUnprocessableEntity)
    }
}

func (oAdmin *OpenvpnAdmin) serverRoleHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, `{"status":"ok", "serverRole": "%s" }`, oAdmin.role)
}

func (oAdmin *OpenvpnAdmin) lastSyncTimeHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Fprint(w, oAdmin.lastSyncTime)
}

func (oAdmin *OpenvpnAdmin) lastSuccessfulSyncTimeHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Fprint(w, oAdmin.lastSuccessfulSyncTime)
}

func (oAdmin *OpenvpnAdmin) downloadCertsHandler(w http.ResponseWriter, r *http.Request) {
	if oAdmin.role == "slave" {
		http.Error(w, `{"status":"error"}`, http.StatusLocked)
		return
	}
	r.ParseForm()
	token := r.Form.Get("token")

	if token != oAdmin.masterSyncToken {
		http.Error(w, `{"status":"error"}`, http.StatusForbidden)
		return
	}

	archiveCerts()
	w.Header().Set("Content-Disposition", "attachment; filename=" + certsArchiveFileName)
	http.ServeFile(w,r, certsArchivePath)
}

func (oAdmin *OpenvpnAdmin) downloadCddHandler(w http.ResponseWriter, r *http.Request) {
	if oAdmin.role == "slave" {
		http.Error(w, `{"status":"error"}`, http.StatusLocked)
		return
	}
	r.ParseForm()
	token := r.Form.Get("token")

	if token != oAdmin.masterSyncToken {
		http.Error(w, `{"status":"error"}`, http.StatusForbidden)
		return
	}

	archiveCcd()
	w.Header().Set("Content-Disposition", "attachment; filename=" + ccdArchiveFileName)
	http.ServeFile(w,r, ccdArchivePath)
}

func main() {
    kingpin.Parse()

	ovpnAdmin := new(OpenvpnAdmin)
	ovpnAdmin.lastSyncTime = "unknown"
	ovpnAdmin.role = *serverRole
	ovpnAdmin.lastSuccessfulSyncTime = "unknown"
	ovpnAdmin.masterSyncToken = *masterSyncToken
	ovpnAdmin.promRegistry = prometheus.NewRegistry()

	ovpnAdmin.registerMetrics()
	ovpnAdmin.setState()

	go ovpnAdmin.updateState()

	if *masterBasicAuthPassword != "" && *masterBasicAuthUser != "" {
		ovpnAdmin.masterHostBasicAuth = true
	} else {
		ovpnAdmin.masterHostBasicAuth = false
	}

	if ovpnAdmin.role == "slave" {
		ovpnAdmin.syncDataFromMaster()
	    go ovpnAdmin.syncWithMaster()
	}

	fs := CacheControlWrapper(http.FileServer(http.Dir(*staticPath)))

	http.Handle("/", fs)
	http.HandleFunc("/api/server/role", ovpnAdmin.serverRoleHandler)
	http.HandleFunc("/api/users/list", ovpnAdmin.userListHandler)
	http.HandleFunc("/api/user/create", ovpnAdmin.userCreateHandler)
	http.HandleFunc("/api/user/revoke", ovpnAdmin.userRevokeHandler)
	http.HandleFunc("/api/user/unrevoke", ovpnAdmin.userUnrevokeHandler)
	http.HandleFunc("/api/user/config/show", ovpnAdmin.userShowConfigHandler)
	http.HandleFunc("/api/user/disconnect", ovpnAdmin.userDisconnectHandler)
	http.HandleFunc("/api/user/statistic", ovpnAdmin.userStatisticHandler)
	http.HandleFunc("/api/user/ccd", ovpnAdmin.userShowCcdHandler)
	http.HandleFunc("/api/user/ccd/apply", ovpnAdmin.userApplyCcdHandler)

	http.HandleFunc("/api/sync/last/try", ovpnAdmin.lastSyncTimeHandler)
	http.HandleFunc("/api/sync/last/successful", ovpnAdmin.lastSuccessfulSyncTimeHandler)
	http.HandleFunc(downloadCertsApiUrl, ovpnAdmin.downloadCertsHandler)
	http.HandleFunc(downloadCcdApiUrl, ovpnAdmin.downloadCddHandler)

	http.Handle(*metricsPath, promhttp.HandlerFor(ovpnAdmin.promRegistry, promhttp.HandlerOpts{}))
	http.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "pong")
	})

	fmt.Println("Bind: http://" + *listenHost + ":" + *listenPort)
	log.Fatal(http.ListenAndServe(*listenHost + ":" + *listenPort, nil))
}

func CacheControlWrapper(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=2592000") // 30 days
		h.ServeHTTP(w, r)
	})
}

func (oAdmin *OpenvpnAdmin) registerMetrics() {
	oAdmin.promRegistry.MustRegister(ovpnServerCertExpire)
	oAdmin.promRegistry.MustRegister(ovpnServerCaCertExpire)
	oAdmin.promRegistry.MustRegister(ovpnClientsTotal)
	oAdmin.promRegistry.MustRegister(ovpnClientsRevoked)
	oAdmin.promRegistry.MustRegister(ovpnClientsConnected)
	oAdmin.promRegistry.MustRegister(ovpnClientsExpired)
	oAdmin.promRegistry.MustRegister(ovpnClientCertificateExpire)
	oAdmin.promRegistry.MustRegister(ovpnClientConnectionInfo)
	oAdmin.promRegistry.MustRegister(ovpnClientConnectionFrom)
	oAdmin.promRegistry.MustRegister(ovpnClientBytesReceived)
	oAdmin.promRegistry.MustRegister(ovpnClientBytesSent)
}

func (oAdmin *OpenvpnAdmin) setState() {
	oAdmin.activeClients = oAdmin.mgmtGetActiveClients()
	oAdmin.clients = oAdmin.usersList()

	ovpnServerCaCertExpire.Set(float64(getOpvnCaCertExpireDate().Unix() - time.Now().Unix() / 3600 / 24))
}

func (oAdmin *OpenvpnAdmin) updateState() {
	for {
		time.Sleep(time.Duration(28) * time.Second)
		ovpnClientBytesSent.Reset()
		ovpnClientBytesReceived.Reset()
		ovpnClientConnectionFrom.Reset()
		ovpnClientConnectionInfo.Reset()
		go oAdmin.setState()
	}
}

func indexTxtParser(txt string) []indexTxtLine {
	var indexTxt []indexTxtLine

	txtLinesArray := strings.Split(txt, "\n")

	for _, v := range txtLinesArray {
		str := strings.Fields(v)
		if len(str) > 0 {
			switch {
			// case strings.HasPrefix(str[0], "E"):
			case strings.HasPrefix(str[0], "V"):
                indexTxt = append(indexTxt, indexTxtLine{Flag: str[0], ExpirationDate: str[1], SerialNumber: str[2], Filename: str[3], DistinguishedName: str[4], Identity: str[4][strings.Index(str[4], "=")+1:]})
			case strings.HasPrefix(str[0], "R"):
                indexTxt = append(indexTxt, indexTxtLine{Flag: str[0], ExpirationDate: str[1], RevocationDate: str[2], SerialNumber: str[3], Filename: str[4], DistinguishedName: str[5], Identity: str[5][strings.Index(str[5], "=")+1:]})
			}
		}
	}

	return indexTxt
}

func renderIndexTxt(data []indexTxtLine) string {
	indexTxt := ""
	for _, line := range data {
		switch {
		case line.Flag == "V":
            indexTxt += fmt.Sprintf("%s\t%s\t\t%s\t%s\t%s\n", line.Flag, line.ExpirationDate, line.SerialNumber, line.Filename, line.DistinguishedName)
		case line.Flag == "R":
            indexTxt += fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s\n", line.Flag, line.ExpirationDate, line.RevocationDate, line.SerialNumber, line.Filename, line.DistinguishedName)
        // case line.flag == "E":
		}
	}
	return indexTxt
}

func (oAdmin *OpenvpnAdmin) renderClientConfig(username string) string {
	if checkUserExist(username) {
		var hosts []OpenvpnServer

		for _, server := range *openvpnServer {
			parts := strings.SplitN(server, ":",2)
			hosts = append(hosts, OpenvpnServer{Host: parts[0], Port: parts[1]})
		}

		conf := openvpnClientConfig{}
		conf.Hosts = hosts
		conf.CA = fRead(*easyrsaDirPath + "/pki/ca.crt")
		conf.Cert = fRead(*easyrsaDirPath + "/pki/issued/" + username + ".crt")
		conf.Key = fRead(*easyrsaDirPath + "/pki/private/" + username + ".key")
		conf.TLS = fRead(*easyrsaDirPath + "/pki/ta.key")

		t, _ := template.ParseFiles("client.conf.tpl")
		var tmp bytes.Buffer
		err := t.Execute(&tmp, conf)
		if err != nil {
			log.Printf("WARNING: something goes wrong during rendering config for %s", username )
		}

		hosts = nil

		fmt.Printf("%+v\n", tmp.String())
		return fmt.Sprintf("%+v\n", tmp.String())
	}
	fmt.Printf("User \"%s\" not found", username)
	return fmt.Sprintf("User \"%s\" not found", username)
}

func (oAdmin *OpenvpnAdmin) parseCcd(username string) Ccd {
	ccd := Ccd{}
	ccd.User = username
	ccd.ClientAddress = "dynamic"
	ccd.CustomRoutes = []ccdRoute{}

	txtLinesArray := strings.Split(fRead(*ccdDir + "/" + username), "\n")

	for _, v := range txtLinesArray {
		str := strings.Fields(v)
		if len(str) > 0 {
			switch {
			case strings.HasPrefix(str[0], "ifconfig-push"):
			    ccd.ClientAddress = str[1]
			case strings.HasPrefix(str[0], "push"):
				ccd.CustomRoutes =  append(ccd.CustomRoutes, ccdRoute{Address: strings.Trim(str[2], "\""), Mask: strings.Trim(str[3], "\""), Description: strings.Trim(strings.Join(str[4:], ""), "#")})
			}
		}
	}

	return ccd
}

func (oAdmin *OpenvpnAdmin) modifyCcd(ccd Ccd) (bool, string) {
	ccdErr := "something goes wrong"

    if fCreate(*ccdDir + "/" + ccd.User) {
        ccdValid, ccdErr := validateCcd(ccd)
        if ccdErr != "" {
		    return false, ccdErr
	    }

        if ccdValid {
            t, _ := template.ParseFiles("ccd.tpl")
            var tmp bytes.Buffer
            tplErr := t.Execute(&tmp, ccd)
			if tplErr != nil {
				log.Println(tplErr)
			}
            fWrite(*ccdDir + "/" + ccd.User, tmp.String())
            return true, "ccd updated successfully"
        }
    }

	return false, ccdErr
}

func validateCcd(ccd Ccd) (bool, string) {
    ccdErr := ""

    if ccd.ClientAddress != "dynamic" {
        _, ovpnNet, err := net.ParseCIDR(*openvpnNetwork)
        if err != nil {
		    log.Println(err)
	    }

	    if ! checkStaticAddressIsFree(ccd.ClientAddress, ccd.User) {
            ccdErr = fmt.Sprintf("ClientAddress \"%s\" already assigned to another user", ccd.ClientAddress)
            if *debug {
                log.Printf("ERROR: Modify ccd for user %s: %s", ccd.User, ccdErr)
            }
            return false, ccdErr
	    }

        if net.ParseIP(ccd.ClientAddress) == nil {
            ccdErr = fmt.Sprintf("ClientAddress \"%s\" not a valid IP address", ccd.ClientAddress)
            if *debug {
                log.Printf("ERROR: Modify ccd for user %s: %s",  ccd.User, ccdErr)
            }
            return false, ccdErr
        }

        if ! ovpnNet.Contains(net.ParseIP(ccd.ClientAddress)) {
            ccdErr = fmt.Sprintf("ClientAddress \"%s\" not belongs to openvpn server network", ccd.ClientAddress)
            if *debug {
                log.Printf("ERROR: Modify ccd for user %s: %s", ccd.User, ccdErr)
            }
            return false, ccdErr
        }
    }

    for _, route := range ccd.CustomRoutes {
        if net.ParseIP(route.Address) == nil {
            ccdErr = fmt.Sprintf("CustomRoute.Address \"%s\" must be a valid IP address", route.Address)
            if *debug {
                log.Printf("ERROR: Modify ccd for user %s: %s", ccd.User, ccdErr)
            }
            return false, ccdErr
        }

        if net.ParseIP(route.Mask) == nil {
            ccdErr = fmt.Sprintf("CustomRoute.Mask \"%s\" must be a valid IP address", route.Mask)
            if *debug {
                log.Printf("ERROR: Modify ccd for user %s: %s", ccd.User, ccdErr)
            }
            return false, ccdErr
        }
    }

	return true, ccdErr
}

func (oAdmin *OpenvpnAdmin) getCcd(username string) Ccd {
	ccd := Ccd{}
	ccd.User = username
	ccd.ClientAddress = "dynamic"
	ccd.CustomRoutes = []ccdRoute{}

    if fCreate(*ccdDir + "/" + username) {
        ccd = oAdmin.parseCcd(username)
    }
    return ccd
}

func checkStaticAddressIsFree(staticAddress string, username string) bool {
    o := runBash(fmt.Sprintf("grep -rl %s %s | grep -vx %s/%s | wc -l", staticAddress, *ccdDir, *ccdDir, username))

    if strings.TrimSpace(o) == "0" {
        return true
    }
    return false
}

func validateUsername(username string) bool {
	var validUsername = regexp.MustCompile(usernameRegexp)
	return validUsername.MatchString(username)
}

func checkUserExist(username string) bool {
	for _, u := range indexTxtParser(fRead(*indexTxtPath)) {
		if u.DistinguishedName == ("/CN=" + username) {
			return true
		}
	}
	return false
}

func (oAdmin *OpenvpnAdmin) usersList() []OpenvpnClient {
	var users []OpenvpnClient

	totalCerts := 0
	validCerts := 0
	revokedCerts := 0
	expiredCerts := 0
	connectedUsers := 0
	apochNow := time.Now().Unix()

	for _, line := range indexTxtParser(fRead(*indexTxtPath)) {
	    if line.Identity != "server" {
			totalCerts += 1
	        ovpnClient := OpenvpnClient{Identity: line.Identity, ExpirationDate: parseDateToString(indexTxtDateLayout, line.ExpirationDate, stringDateFormat)}
            switch {
                case line.Flag == "V":
                    ovpnClient.AccountStatus = "Active"
					ovpnClientCertificateExpire.WithLabelValues(line.Identity).Set(float64((parseDateToUnix(indexTxtDateLayout, line.ExpirationDate) - apochNow) / 3600 / 24))
					validCerts += 1
			case line.Flag == "R":
                    ovpnClient.AccountStatus = "Revoked"
                    ovpnClient.RevocationDate = parseDateToString(indexTxtDateLayout, line.RevocationDate, stringDateFormat)
					ovpnClientCertificateExpire.WithLabelValues(line.Identity).Set(float64((parseDateToUnix(indexTxtDateLayout, line.ExpirationDate) - apochNow) / 3600 / 24))
					revokedCerts += 1
                case line.Flag == "E":
                    ovpnClient.AccountStatus = "Expired"
					ovpnClientCertificateExpire.WithLabelValues(line.Identity).Set(float64((parseDateToUnix(indexTxtDateLayout, line.ExpirationDate) - apochNow) / 3600 / 24))
					expiredCerts += 1
            }

            if isUserConnected(line.Identity, oAdmin.activeClients) {
                ovpnClient.ConnectionStatus = "Connected"
				connectedUsers += 1
            }

            users = append(users, ovpnClient)

        } else {
			ovpnServerCertExpire.Set(float64((parseDateToUnix(indexTxtDateLayout, line.ExpirationDate) - apochNow) / 3600 / 24))
		}
	}

	otherCerts := totalCerts - validCerts - revokedCerts - expiredCerts

	if otherCerts != 0 {
		log.Printf("WARNING: there are %d otherCerts", otherCerts)
	}

	ovpnClientsTotal.Set(float64(totalCerts))
	ovpnClientsRevoked.Set(float64(revokedCerts))
	ovpnClientsExpired.Set(float64(expiredCerts))
	ovpnClientsConnected.Set(float64(connectedUsers))

	return users
}

func (oAdmin *OpenvpnAdmin) userCreate(username string) (bool, string) {
    ucErr := fmt.Sprintf("User \"%s\" created", username)
    // TODO: add password for user cert . priority=low
	if validateUsername(username) == false {
		ucErr = fmt.Sprintf("Username \"%s\" incorrect, you can use only %s\n", username, usernameRegexp)
        if *debug {
            log.Printf("ERROR: userCreate: %s", ucErr)
        }
		return false, ucErr
	}
	if checkUserExist(username) {
		ucErr = fmt.Sprintf("User \"%s\" already exists\n", username)
        if *debug {
            log.Printf("ERROR: userCreate: %s", ucErr)
        }
		return false, ucErr
	}
	o := runBash(fmt.Sprintf("date +%%Y-%%m-%%d\\ %%H:%%M:%%S && cd %s && easyrsa build-client-full %s nopass", *easyrsaDirPath, username))
	fmt.Println(o)
	if *debug {
		log.Printf("INFO: user created: %s", username)
	}
	oAdmin.clients = oAdmin.usersList()
	return true, ucErr
}

func (oAdmin *OpenvpnAdmin) getUserStatistic(username string) clientStatus {
	for _, u := range oAdmin.activeClients {
		if u.CommonName == username {
			return u
		}
	}
	return clientStatus{}
}

func (oAdmin *OpenvpnAdmin) userRevoke(username string) string {
	if checkUserExist(username) {
		// check certificate valid flag 'V'
		o := runBash(fmt.Sprintf("date +%%Y-%%m-%%d\\ %%H:%%M:%%S && cd %s && echo yes | easyrsa revoke %s && easyrsa gen-crl", *easyrsaDirPath, username))
		crlFix()
		oAdmin.clients = oAdmin.usersList()
		return fmt.Sprintln(o)
	}
	fmt.Printf("User \"%s\" not found", username)
	return fmt.Sprintf("User \"%s\" not found", username)
}

func (oAdmin *OpenvpnAdmin) userUnrevoke(username string) string {
	if checkUserExist(username) {
		// check certificate revoked flag 'R'
		usersFromIndexTxt := indexTxtParser(fRead(*indexTxtPath))
		for i := range usersFromIndexTxt {
			if usersFromIndexTxt[i].DistinguishedName == ("/CN=" + username) {
			    if usersFromIndexTxt[i].Flag == "R" {
                    usersFromIndexTxt[i].Flag = "V"
                    usersFromIndexTxt[i].RevocationDate = ""
                    o := runBash(fmt.Sprintf("cd %s && cp pki/revoked/certs_by_serial/%s.crt pki/issued/%s.crt", *easyrsaDirPath, usersFromIndexTxt[i].SerialNumber, username))
                    //fmt.Println(o)
                    o = runBash(fmt.Sprintf("cd %s && cp pki/revoked/certs_by_serial/%s.crt pki/certs_by_serial/%s.pem", *easyrsaDirPath, usersFromIndexTxt[i].SerialNumber, usersFromIndexTxt[i].SerialNumber))
                    //fmt.Println(o)
                    o = runBash(fmt.Sprintf("cd %s && cp pki/revoked/private_by_serial/%s.key pki/private/%s.key", *easyrsaDirPath, usersFromIndexTxt[i].SerialNumber, username))
                    //fmt.Println(o)
                    o = runBash(fmt.Sprintf("cd %s && cp pki/revoked/reqs_by_serial/%s.req pki/reqs/%s.req", *easyrsaDirPath, usersFromIndexTxt[i].SerialNumber, username))
                    //fmt.Println(o)
                    fWrite(*indexTxtPath, renderIndexTxt(usersFromIndexTxt))
                    //fmt.Print(renderIndexTxt(usersFromIndexTxt))
                    o = runBash(fmt.Sprintf("cd %s && easyrsa gen-crl", *easyrsaDirPath))
                    //fmt.Println(o)
                    crlFix()
                    o = ""
					fmt.Println(o)
                    break
                }
			}
		}
		fWrite(*indexTxtPath, renderIndexTxt(usersFromIndexTxt))
		fmt.Print(renderIndexTxt(usersFromIndexTxt))
		crlFix()
		oAdmin.clients = oAdmin.usersList()
		return fmt.Sprintf("{\"msg\":\"User %s successfully unrevoked\"}", username)
	}
	return fmt.Sprintf("{\"msg\":\"User \"%s\" not found\"}", username)
}

// TODO: add ability to change password for user cert . priority=low
func userChangePassword(username string, newPassword string) bool {

    return false
}

func (oAdmin *OpenvpnAdmin) mgmtRead(conn net.Conn) string {
	buf := make([]byte, 32768)
	bufLen, _ := conn.Read(buf)
	s := string(buf[:bufLen])
	return s
}

func (oAdmin *OpenvpnAdmin) mgmtConnectedUsersParser(text string) []clientStatus {
	var u []clientStatus
	isClientList := false
	isRouteTable := false
	scanner := bufio.NewScanner(strings.NewReader(text))
	for scanner.Scan() {
		txt := scanner.Text()
		if regexp.MustCompile(`^Common Name,Real Address,Bytes Received,Bytes Sent,Connected Since$`).MatchString(txt) {
			isClientList = true
			continue
		}
		if regexp.MustCompile(`^ROUTING TABLE$`).MatchString(txt) {
			isClientList = false
			continue
		}
		if regexp.MustCompile(`^Virtual Address,Common Name,Real Address,Last Ref$`).MatchString(txt) {
			isRouteTable = true
			continue
		}
		if regexp.MustCompile(`^GLOBAL STATS$`).MatchString(txt) {
			// isRouteTable = false // ineffectual assignment to isRouteTable (ineffassign)
			break
		}
		if isClientList {
			user := strings.Split(txt, ",")

			userName := user[0]
			userAddress := user[1]
			userBytesRecieved:= user[2]
			userBytesSent:= user[3]
			userConnectedSince := user[4]

			userStatus := clientStatus{CommonName: userName, RealAddress: userAddress, BytesReceived: userBytesRecieved, BytesSent: userBytesSent, ConnectedSince: userConnectedSince}
			u = append(u, userStatus)
			bytesSent, _ := strconv.Atoi(userBytesSent)
			bytesReceive, _ := strconv.Atoi(userBytesRecieved)
			ovpnClientConnectionFrom.WithLabelValues(userName, userAddress).Set(float64(parseDateToUnix(ovpnStatusDateLayout, userConnectedSince)))
			ovpnClientBytesSent.WithLabelValues(userName).Set(float64(bytesSent))
			ovpnClientBytesReceived.WithLabelValues(userName).Set(float64(bytesReceive))
		}
		if isRouteTable {
			user := strings.Split(txt, ",")
			for i := range u {
				if u[i].CommonName == user[1] {
					u[i].VirtualAddress = user[0]
					u[i].LastRef = user[3]
					ovpnClientConnectionInfo.WithLabelValues(user[1], user[0]).Set(float64(parseDateToUnix(ovpnStatusDateLayout, user[3])))
					break
				}
			}
		}
	}
	return u
}

func (oAdmin *OpenvpnAdmin) mgmtKillUserConnection(username string) {
	conn, err := net.Dial("tcp", *mgmtListenHost+":"+*mgmtListenPort)
	if err != nil {
		log.Println("ERROR: openvpn mgmt interface is not reachable")
		return
	}
	oAdmin.mgmtRead(conn) // read welcome message
	conn.Write([]byte(fmt.Sprintf("kill %s\n", username)))
	fmt.Printf("%v", oAdmin.mgmtRead(conn))
	conn.Close()
}

func (oAdmin *OpenvpnAdmin) mgmtGetActiveClients() []clientStatus {
	conn, err := net.Dial("tcp", *mgmtListenHost+":"+*mgmtListenPort)
	if err != nil {
		log.Println("ERROR: openvpn mgmt interface is not reachable")
		return []clientStatus{}
	}
	oAdmin.mgmtRead(conn) // read welcome message
	conn.Write([]byte("status\n"))
	activeClients := oAdmin.mgmtConnectedUsersParser(oAdmin.mgmtRead(conn))
	conn.Close()
	return activeClients
}

func isUserConnected(username string, connectedUsers []clientStatus) bool {
    for _, connectedUser := range connectedUsers {
        if connectedUser.CommonName == username {
            return true
        }
    }
    return false
}

func (oAdmin *OpenvpnAdmin) downloadCerts() bool {
	if fExist(certsArchivePath) {
		fDelete(certsArchivePath)
	}
    err := fDownload(certsArchivePath, *masterHost + downloadCertsApiUrl + "?token=" + oAdmin.masterSyncToken, oAdmin.masterHostBasicAuth)
    if err != nil {
		log.Println(err)
		return false
	}

	return true
}

func (oAdmin *OpenvpnAdmin) downloadCcd() bool {
	if fExist(ccdArchivePath) {
		fDelete(ccdArchivePath)
	}

	err := fDownload(ccdArchivePath, *masterHost + downloadCcdApiUrl + "?token=" + oAdmin.masterSyncToken, oAdmin.masterHostBasicAuth)
	if err != nil {
		log.Println(err)
		return false
	}

	return true
}

func archiveCerts() {
	o := runBash(fmt.Sprintf("cd %s && tar -czf %s *", *easyrsaDirPath + "/pki", certsArchivePath ))
	fmt.Println(o)
}

func archiveCcd() {
	o := runBash(fmt.Sprintf("cd %s && tar -czf %s *", *ccdDir, ccdArchivePath ))
	fmt.Println(o)
}

func unArchiveCerts() {
	runBash(fmt.Sprintf("mkdir -p %s", *easyrsaDirPath + "/pki"))
	o := runBash(fmt.Sprintf("cd %s && tar -xzf %s", *easyrsaDirPath + "/pki", certsArchivePath ))
	fmt.Println(o)
}

func unArchiveCcd() {
	runBash(fmt.Sprintf("mkdir -p %s", *ccdDir))
	o := runBash(fmt.Sprintf("cd %s && tar -xzf %s", *ccdDir, ccdArchivePath ))
	fmt.Println(o)
}

func (oAdmin *OpenvpnAdmin) syncDataFromMaster() {
	retryCountMax := 3
	certsDownloadFailed := true
	ccdDownloadFailed := true
	certsDownloadRetries := 0
	ccdDownloadRetries := 0

	for certsDownloadFailed && certsDownloadRetries < retryCountMax {
		certsDownloadRetries += 1
		log.Printf("Downloading certs archive from master. Attempt %d", certsDownloadRetries)
		if oAdmin.downloadCerts() {
			certsDownloadFailed = false
			log.Println("Decompression certs archive from master")
			unArchiveCerts()
		} else {
			log.Printf("WARNING: something goes wrong during downloading certs from master. Attempt %d", certsDownloadRetries)
		}
	}

	for ccdDownloadFailed && ccdDownloadRetries < retryCountMax {
		ccdDownloadRetries += 1
		log.Printf("Downloading ccd archive from master. Attempt %d", ccdDownloadRetries)
		if oAdmin.downloadCcd() {
			ccdDownloadFailed = false
			log.Println("Decompression ccd archive from master")
			unArchiveCcd()
		} else {
			log.Printf("WARNING: something goes wrong during downloading certs from master. Attempt %d", ccdDownloadRetries)
		}
	}

	oAdmin.lastSyncTime = time.Now().Format("2006-01-02 15:04:05")
	if !ccdDownloadFailed && !certsDownloadFailed {
		oAdmin.lastSuccessfulSyncTime = time.Now().Format("2006-01-02 15:04:05")
	}
}

func (oAdmin *OpenvpnAdmin) syncWithMaster() {
    for {
		time.Sleep(time.Duration(*masterSyncFrequency) * time.Second)
		oAdmin.syncDataFromMaster()
    }
}

func getOpvnCaCertExpireDate() time.Time {
	caCertPath := *easyrsaDirPath + "/pki/ca.crt"
	caCertExpireDate := runBash(fmt.Sprintf("openssl x509 -in %s -noout -enddate | awk -F \"=\" {'print $2'}", caCertPath))

	dateLayout := "Jan 2 15:04:05 2006 MST"
	t, err := time.Parse(dateLayout, strings.TrimSpace(caCertExpireDate))
	if err != nil {
		log.Printf("WARNING: can`t parse expire date for CA cert: %v", err)
		return time.Now()
	}

	return t
}

// https://community.openvpn.net/openvpn/ticket/623
func crlFix() {
	err1 := os.Chmod(*easyrsaDirPath + "/pki", 0755)
	if err1 != nil {
		log.Println(err1)
	}
	err2 := os.Chmod(*easyrsaDirPath + "/pki/crl.pem", 0644)
	if err2 != nil {
		log.Println(err2)
	}
}