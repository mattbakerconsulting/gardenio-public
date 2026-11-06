# oksvg survey: Noto Emoji v2026-09-24-unicode18_0, single-codepoint SVGs

Each SVG rasterised by oksvg at 72×72 and compared, on white, with the release's own 72-px PNG of the same emoji (mean absolute channel difference).

| | count |
|---|---|
| SVGs surveyed | 1402 |
| parsed in StrictErrorMode | 1222 |
| parsed only in IgnoreErrorMode (something dropped) | 66 |
| failed to parse at all | 114 |
| rendered, diff ≤ 2% (matches the PNG) | 821 |
| rendered, diff 2–8% (visibly off at 64 px) | 235 |
| rendered, diff > 8% (wrong) | 232 |

Diff percentiles: p50 1.06%, p90 16.71%, p99 37.33%, max 78.47%.

## Strict-mode errors, by message

| count | message |
|---|---|
| 76 | `XML syntax error on line 13: invalid character entity &ns_extend;` |
| 66 | `Cannot process svg element clipPath` |
| 38 | `XML syntax error on line 14: invalid character entity &ns_extend;` |

## Worst 25 by diff

| emoji | file | strict | diff |
|---|---|---|---|
| 🌫 | emoji_u1f32b.svg | true | 78.5% |
| 🌁 | emoji_u1f301.svg | true | 74.1% |
| 🫓 | emoji_u1fad3.svg | true | 53.0% |
| 🌝 | emoji_u1f31d.svg | true | 52.4% |
| 🌅 | emoji_u1f305.svg | true | 51.2% |
| 💀 | emoji_u1f480.svg | true | 43.6% |
| 🫪 | emoji_u1faea.svg | true | 42.0% |
| 🌍 | emoji_u1f30d.svg | true | 39.5% |
| 🧊 | emoji_u1f9ca.svg | true | 38.5% |
| 💷 | emoji_u1f4b7.svg | false | 38.1% |
| 🌪 | emoji_u1f32a.svg | false | 38.0% |
| 💰 | emoji_u1f4b0.svg | false | 37.9% |
| ☄ | emoji_u2604.svg | true | 37.6% |
| 🌏 | emoji_u1f30f.svg | true | 37.3% |
| 🌄 | emoji_u1f304.svg | true | 36.7% |
| 🪟 | emoji_u1fa9f.svg | true | 36.7% |
| 💶 | emoji_u1f4b6.svg | false | 36.1% |
| 🪦 | emoji_u1faa6.svg | true | 36.1% |
| 🫂 | emoji_u1fac2.svg | true | 35.7% |
| 🌎 | emoji_u1f30e.svg | true | 35.6% |
| 🧅 | emoji_u1f9c5.svg | true | 35.2% |
| 🏜 | emoji_u1f3dc.svg | true | 35.1% |
| 💵 | emoji_u1f4b5.svg | false | 34.2% |
| 🍑 | emoji_u1f351.svg | true | 33.7% |
| ✊ | emoji_u270a.svg | false | 33.6% |

## Failed to parse

- 💲 emoji_u1f4b2.svg: `XML syntax error on line 14: invalid character entity &ns_extend;`
- 🔊 emoji_u1f50a.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- 🎦 emoji_u1f3a6.svg: `XML syntax error on line 14: invalid character entity &ns_extend;`
- 💉 emoji_u1f489.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- 🎟 emoji_u1f39f.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- 🎩 emoji_u1f3a9.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- 🎪 emoji_u1f3aa.svg: `XML syntax error on line 14: invalid character entity &ns_extend;`
- 💱 emoji_u1f4b1.svg: `XML syntax error on line 14: invalid character entity &ns_extend;`
- 🥒 emoji_u1f952.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- 👛 emoji_u1f45b.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- 🥍 emoji_u1f94d.svg: `XML syntax error on line 14: invalid character entity &ns_extend;`
- 👔 emoji_u1f454.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- 💻 emoji_u1f4bb.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- ◾ emoji_u25fe.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- ◽ emoji_u25fd.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- 🤰 emoji_u1f930.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- ◀ emoji_u25c0.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- 📖 emoji_u1f4d6.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- 📚 emoji_u1f4da.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- 📞 emoji_u1f4de.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- 📢 emoji_u1f4e2.svg: `XML syntax error on line 14: invalid character entity &ns_extend;`
- 📣 emoji_u1f4e3.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- ▶ emoji_u25b6.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- ▫ emoji_u25ab.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- ▪ emoji_u25aa.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- ⏺ emoji_u23fa.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- ⏹ emoji_u23f9.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- 🎛 emoji_u1f39b.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- ⏸ emoji_u23f8.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- 🍡 emoji_u1f361.svg: `XML syntax error on line 14: invalid character entity &ns_extend;`
- 📳 emoji_u1f4f3.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- 📶 emoji_u1f4f6.svg: `XML syntax error on line 14: invalid character entity &ns_extend;`
- 📺 emoji_u1f4fa.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- 🔀 emoji_u1f500.svg: `XML syntax error on line 14: invalid character entity &ns_extend;`
- 🔁 emoji_u1f501.svg: `XML syntax error on line 14: invalid character entity &ns_extend;`
- 🎬 emoji_u1f3ac.svg: `XML syntax error on line 14: invalid character entity &ns_extend;`
- 🔂 emoji_u1f502.svg: `XML syntax error on line 14: invalid character entity &ns_extend;`
- 🔄 emoji_u1f504.svg: `XML syntax error on line 14: invalid character entity &ns_extend;`
- ⏯ emoji_u23ef.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- ⏮ emoji_u23ee.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- ⏭ emoji_u23ed.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- ⏬ emoji_u23ec.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- ⏫ emoji_u23eb.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- ⏪ emoji_u23ea.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- ⏩ emoji_u23e9.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- ⏏ emoji_u23cf.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- 🐼 emoji_u1f43c.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- 🧈 emoji_u1f9c8.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- 🐶 emoji_u1f436.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- 🐳 emoji_u1f433.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- 🐭 emoji_u1f42d.svg: `XML syntax error on line 14: invalid character entity &ns_extend;`
- 🐨 emoji_u1f428.svg: `XML syntax error on line 14: invalid character entity &ns_extend;`
- 🔈 emoji_u1f508.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- ⛔ emoji_u26d4.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- 🔉 emoji_u1f509.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- 🛶 emoji_u1f6f6.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- ⛰ emoji_u26f0.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- 🥢 emoji_u1f962.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- 🔖 emoji_u1f516.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- 🛙 emoji_u1f6d9.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- 🔘 emoji_u1f518.svg: `XML syntax error on line 14: invalid character entity &ns_extend;`
- 🛎 emoji_u1f6ce.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- ⛷ emoji_u26f7.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- 🔦 emoji_u1f526.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- 🌇 emoji_u1f307.svg: `XML syntax error on line 14: invalid character entity &ns_extend;`
- 🌆 emoji_u1f306.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- 🍁 emoji_u1f341.svg: `XML syntax error on line 14: invalid character entity &ns_extend;`
- 🫺 emoji_u1fafa.svg: `XML syntax error on line 14: invalid character entity &ns_extend;`
- 🚰 emoji_u1f6b0.svg: `XML syntax error on line 14: invalid character entity &ns_extend;`
- 🫹 emoji_u1faf9.svg: `XML syntax error on line 14: invalid character entity &ns_extend;`
- 🎀 emoji_u1f380.svg: `XML syntax error on line 14: invalid character entity &ns_extend;`
- 🧽 emoji_u1f9fd.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- 🚪 emoji_u1f6aa.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- 🐋 emoji_u1f40b.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- 🚦 emoji_u1f6a6.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- 🚥 emoji_u1f6a5.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- 🪋 emoji_u1fa8b.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- 🚣 emoji_u1f6a3.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- 🚢 emoji_u1f6a2.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- 🫫 emoji_u1faeb.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- 🍽 emoji_u1f37d.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- 🪍 emoji_u1fa8d.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- 🪐 emoji_u1fa90.svg: `XML syntax error on line 14: invalid character entity &ns_extend;`
- ✴ emoji_u2734.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- 🏸 emoji_u1f3f8.svg: `XML syntax error on line 14: invalid character entity &ns_extend;`
- 🍧 emoji_u1f367.svg: `XML syntax error on line 14: invalid character entity &ns_extend;`
- 🏵 emoji_u1f3f5.svg: `XML syntax error on line 14: invalid character entity &ns_extend;`
- 🪞 emoji_u1fa9e.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- 🔼 emoji_u1f53c.svg: `XML syntax error on line 14: invalid character entity &ns_extend;`
- 🪡 emoji_u1faa1.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- 🍨 emoji_u1f368.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- 🏫 emoji_u1f3eb.svg: `XML syntax error on line 14: invalid character entity &ns_extend;`
- 🪤 emoji_u1faa4.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- 🏩 emoji_u1f3e9.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- 🔽 emoji_u1f53d.svg: `XML syntax error on line 14: invalid character entity &ns_extend;`
- 🏨 emoji_u1f3e8.svg: `XML syntax error on line 14: invalid character entity &ns_extend;`
- 🕋 emoji_u1f54b.svg: `XML syntax error on line 14: invalid character entity &ns_extend;`
- 🕳 emoji_u1f573.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- 🚎 emoji_u1f68e.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- 🕸 emoji_u1f578.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- 🪧 emoji_u1faa7.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- 🍩 emoji_u1f369.svg: `XML syntax error on line 14: invalid character entity &ns_extend;`
- 🚀 emoji_u1f680.svg: `XML syntax error on line 14: invalid character entity &ns_extend;`
- 🏙 emoji_u1f3d9.svg: `XML syntax error on line 14: invalid character entity &ns_extend;`
- 🖲 emoji_u1f5b2.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- 🏘 emoji_u1f3d8.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- 🏓 emoji_u1f3d3.svg: `XML syntax error on line 14: invalid character entity &ns_extend;`
- 🏑 emoji_u1f3d1.svg: `XML syntax error on line 14: invalid character entity &ns_extend;`
- 🏏 emoji_u1f3cf.svg: `XML syntax error on line 14: invalid character entity &ns_extend;`
- 🗺 emoji_u1f5fa.svg: `XML syntax error on line 14: invalid character entity &ns_extend;`
- 😻 emoji_u1f63b.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- 😹 emoji_u1f639.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- 🍴 emoji_u1f374.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
- 😸 emoji_u1f638.svg: `XML syntax error on line 13: invalid character entity &ns_extend;`
