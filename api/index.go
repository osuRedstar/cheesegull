package api

import (
	"expvar"
	"io/ioutil"
)

// Version is set by main and it is given to requests at /
var Version = "v2.DEV"
var page string

func index(c *Context) {
	c.WriteHeader("Content-Type", "text/html; charset=utf-8")
	if len(page) < 1 {
		data, err := ioutil.ReadFile("page.html")
		if err != nil {
			page = "CheeseGull " + Version + " Woo\nFor more information: https://github.com/osuRedstar/cheesegull"
			return
		}

		page = string(data)
	}

	c.Write([]byte(page))
}

var _evh = expvar.Handler()

func expvarHandler(c *Context) {
	//_evh.ServeHTTP(c.writer, c.Request)
	c.WriteJSON(403, "NAGA!")
}

func init() {
	GET("/", index)
	GET("/expvar", expvarHandler)
}
