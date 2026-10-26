# Vision test image

One real photo, pulled at random from [cataas.com](https://cataas.com) (Cat as a
Service), for checking the image path by hand.

1. Open the UI (`dsagent.exe`) and switch the model to `deepseek-flash`.
2. Click **+** in the composer, then **Add Image...**, and pick `cat.jpg` here.
   It is also served at `http://127.0.0.1:8787/test/cat.jpg`.
3. Ask "what is in this picture?".

A reply that describes a cat means the whole path works: the file picker, the data
URL, the model check and the image content blocks. Anything else, and the error
says which of the four broke.

It is a real JPEG bitmap from an actual cat, which matters: the server takes the
content type from the bytes rather than the file name, so a drawn placeholder
would not exercise that check.

`testdata/vision/` holds the small PNG/JPEG pair the automated tests use
(`vision_test.go`) - keep this folder for the manual demo only.

Note: everything under `ui/` is embedded into the executable by `//go:embed ui/*`,
so this file adds about 18 KB to the binary. Delete it if that matters more than
the convenience.
