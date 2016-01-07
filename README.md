# Multidown

Downloads files over HTTP with multiple streams to assuage rate limiting. Unlike [Axel](http://axel.alioth.debian.org/), this splits the file up into small chunks so that the beginning of the file finishes as fast as the multiple streams instead splitting the file into one chunk per processor. The main purpose is streaming video over a prohibitively slow connection.

## Example Usage

First build the executable (no external dependencies besides golang)

    $ go build main.go

Download a file

    $ multidown http://myslowwebsite.com/file.mp4

Download a file that outputs to `myvideofile.mp4`

    $ multidown -o myvideofile.mp4 http://myslowwebsite.com/file.mp4

Download a file with 10 streams at once that outputs to `myvideofile.mp4`

    $ multidown -n 10 -o myvideofile.mp4 http://myslowwebsite.com/file.mp4
