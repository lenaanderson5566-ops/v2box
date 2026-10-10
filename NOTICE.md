# Third-party notices

This project derives from wyx2685/v2node. The original MPL-2.0 license is preserved in LICENSE; existing source files retain their applicable terms.

The backend embeds SagerNet/sing-box v1.14.3 (GPL-3.0-or-later). Its copyright and license are reproduced in LICENSE.sing-box. Distributions of the combined executable must comply with the applicable GPL requirements, including providing corresponding source. Keep these notices and both license files with binary releases. New core adapter files are provided under GPL-3.0-or-later.

Other Go dependencies retain their respective licenses; their exact versions are recorded in go.mod and go.sum.

Local adaptations of sing-box, sing-vmess, sing-quic and sing-anytls are included
under internal/. Their origins and modifications are documented in
internal/README.md and their license/copyright texts in internal/licenses/.
These files are part of the corresponding source and their notices accompany
binary packages under licenses/.
