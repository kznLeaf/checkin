.PHONY: build run clean

build: checkin

run: checkin
	./bin/checkin

clean:
	rm -rf ./bin/

checkin:
	go build -v -o bin/checkin .

