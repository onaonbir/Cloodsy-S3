# Third-party licenses

Cloodsy S3 itself is distributed under the
[Cloodsy S3 Community License 1.0](LICENSE). The binary statically links the
open-source Go modules below. Their license texts are included in the Go
module cache (`go mod download -json <module>` prints the location) and in the
respective upstream repositories.

Regenerate this list after changing `go.mod` (`go list -m all`).

## Direct dependencies

| Module | Version | License | Purpose |
|--------|---------|---------|---------|
| github.com/disintegration/imaging | v1.6.2 | MIT | image resizing / re-encoding |
| github.com/google/uuid | v1.6.0 | BSD-3-Clause | upload and version identifiers |
| github.com/pterm/pterm | v0.12.83 | MIT | CLI output |
| golang.org/x/crypto | v0.55.0 | BSD-3-Clause | bcrypt (admin passwords) |
| golang.org/x/image | v0.45.0 | BSD-3-Clause | WebP decoding |
| golang.org/x/net | v0.58.0 | BSD-3-Clause | WebDAV server |
| golang.org/x/term | v0.45.0 | BSD-3-Clause | no-echo password prompt |
| gopkg.in/yaml.v3 | v3.0.1 | MIT (parts Apache-2.0, derived from libyaml) | configuration parsing |
| modernc.org/sqlite | v1.46.1 | BSD-3-Clause | embedded SQLite (pure Go) |

## Indirect dependencies

| Module | Version | License |
|--------|---------|---------|
| atomicgo.dev/cursor | v0.2.0 | MIT |
| atomicgo.dev/keyboard | v0.2.9 | MIT |
| atomicgo.dev/schedule | v0.1.0 | MIT |
| github.com/clipperhouse/uax29/v2 | v2.7.0 | MIT |
| github.com/containerd/console | v1.0.5 | Apache-2.0 |
| github.com/dustin/go-humanize | v1.0.1 | MIT |
| github.com/gookit/color | v1.6.0 | MIT |
| github.com/lithammer/fuzzysearch | v1.1.8 | MIT |
| github.com/mattn/go-isatty | v0.0.20 | MIT |
| github.com/mattn/go-runewidth | v0.0.20 | MIT |
| github.com/ncruces/go-strftime | v1.0.0 | MIT |
| github.com/remyoudompheng/bigfft | (pseudo-version) | BSD-3-Clause |
| github.com/xo/terminfo | (pseudo-version) | MIT |
| golang.org/x/exp | (pseudo-version) | BSD-3-Clause |
| golang.org/x/sys | v0.47.0 | BSD-3-Clause |
| golang.org/x/text | v0.41.0 | BSD-3-Clause |
| modernc.org/libc | v1.67.6 | BSD-3-Clause |
| modernc.org/mathutil | v1.7.1 | BSD-3-Clause |
| modernc.org/memory | v1.11.0 | BSD-3-Clause |

## Runtime images

The Docker image is based on `gcr.io/distroless/static-debian12`
(Apache-2.0, Debian packages under their respective licenses).

## License texts

### MIT License

```
Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```

### BSD 3-Clause License

```
Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are met:

1. Redistributions of source code must retain the above copyright notice, this
   list of conditions and the following disclaimer.
2. Redistributions in binary form must reproduce the above copyright notice,
   this list of conditions and the following disclaimer in the documentation
   and/or other materials provided with the distribution.
3. Neither the name of the copyright holder nor the names of its contributors
   may be used to endorse or promote products derived from this software
   without specific prior written permission.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS"
AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE
IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE ARE
DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT HOLDER OR CONTRIBUTORS BE LIABLE
FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL
DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR
SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER
CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY,
OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
```

### Apache License 2.0

Full text: <https://www.apache.org/licenses/LICENSE-2.0>
