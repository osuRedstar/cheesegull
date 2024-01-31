#lets/config.ini --> [cheesegull] apiurl = http://localhost:6201/api

import os
import tornado.ioloop
import tornado.web

port = 6201
def redirectServer(self):
    return f"http://124.58.110.110:{port}{self.request.uri}"
    #return f"https://cheesegull.redstar.moe{self.request.uri}"

class MainHandler(tornado.web.RequestHandler):
    def get(self):
        redirectServerUrl = redirectServer(self)
        print(f"\nhttp://localhost:{port}{self.request.uri} --> {redirectServerUrl}")
        self.redirect(redirectServerUrl)

def make_app():
    return tornado.web.Application([
        (r"/.*", MainHandler)
    ])

if __name__ == "__main__":
    debugMode = True
    app = make_app()
    app.listen(port)
    print(f"Server Listen on http://localhost:{port} Port | OS = {'Windows' if os.name == 'nt' else 'UNIX'}")
    tornado.ioloop.IOLoop.current().start()