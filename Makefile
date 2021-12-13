.POSIX:

PROG = snap

build:
	go build -o $(PROG) .

install:
	install -DT -m 755 $(PROG) $(DESTDIR)$(PREFIX)/bin/snap

clean:
	rm -f -- $(PROG)
