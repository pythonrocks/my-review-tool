# Version from the nearest v* tag (v1.2.3 -> 1.2.3, v1.2.0-rc1 -> 1.2.0); 0.0.0 when there is none.
VERSION := $(shell v=$$(git describe --tags --abbrev=0 --match 'v*' 2>/dev/null); echo $${v:-v0.0.0} | sed 's/^v//; s/-.*//')
APP := bin/myReview.app
# The minimum macOS version Wails v3 supports (its darwin Taskfile and Info.plist template).
MACOS_MIN := 12.0

run:
	go run .

test:
	go test ./...

lint:
	test -z "$$(gofmt -l .)"
	go vet ./...
	GOOS=linux GOARCH=arm64 go vet -tags purego ./... # Wails needs cgo or purego; no cross cgo here

# The steps of Wails v3's darwin package:universal task, without needing the wails3 CLI and Task.
app:
	rm -rf $(APP) && mkdir -p $(APP)/Contents/MacOS $(APP)/Contents/Resources
	for arch in arm64 amd64; do \
		GOOS=darwin GOARCH=$$arch CGO_ENABLED=1 MACOSX_DEPLOYMENT_TARGET=$(MACOS_MIN) \
		CGO_CFLAGS=-mmacosx-version-min=$(MACOS_MIN) CGO_LDFLAGS=-mmacosx-version-min=$(MACOS_MIN) \
		go build -tags production -trimpath -buildvcs=false -ldflags="-w -s" -o bin/myReview-$$arch . || exit 1; \
	done
	lipo -create -output $(APP)/Contents/MacOS/myReview bin/myReview-arm64 bin/myReview-amd64
	rm bin/myReview-arm64 bin/myReview-amd64
	cp build/darwin/icons.icns LICENSE.md THIRD_PARTY_NOTICES.md $(APP)/Contents/Resources/
	sed 's/@VERSION@/$(VERSION)/g' build/darwin/Info.plist > $(APP)/Contents/Info.plist
	plutil -lint $(APP)/Contents/Info.plist
	codesign --force --deep --sign - $(APP) # ad hoc, as Wails does: arm64 code must be signed; not a Developer ID
	lipo -info $(APP)/Contents/MacOS/myReview

dmg:
	rm -rf bin/dmg bin/myReview.dmg && mkdir bin/dmg
	cp -R $(APP) bin/dmg/
	ln -s /Applications bin/dmg/Applications
	hdiutil create -volname myReview -srcfolder bin/dmg -fs HFS+ -format UDZO bin/myReview.dmg
	rm -rf bin/dmg

.PHONY: run test lint app dmg
