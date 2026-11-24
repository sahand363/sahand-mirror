# سهند میرور (sahand-mirror)

هاب داخلی برای روزهای قطعی اینترنت بین‌الملل. برنامه‌هایی که معمولاً از گیت‌هاب نصب می‌شوند (3x-ui، zimzim، loadinme، blendartai و …) را — با ریلیزها، اسکریپت نصب، سورس و فایل‌های لازم — روی یک سرور داخل ایران آینه می‌کند تا وقتی فقط اینترنت ملی کار می‌کند، روی هر سرور دیگری با یک خط نصبشان کنید. اسکریپت‌های نصب **بدون تغییر** اجرا می‌شوند: هاب خودش نقش github.com، api.github.com و raw.githubusercontent.com را بازی می‌کند.

- یک فایل اجرایی Go (فقط کتابخانهٔ استاندارد)، پنل وب فارسی و راست‌به‌چپ، بدون CDN و فونت خارجی
- تنظیمات: `/etc/sahand-mirror/config.json` · داده‌ها: `/var/lib/sahand-mirror` · باینری: `/usr/local/bin/sahand-mirror`
- سرویس systemd + منوی مدیریتی (`sahand-mirror` با دسترسی root)
- **نسخهٔ ۰٫۳:** فهرست عمومی برنامه‌ها (`/catalog`)، README هر برنامه (`/catalog/<id>/readme`) و نصب‌کنندهٔ [Sahand Mirror Companion](https://github.com/sahand363/sahand-mirror-companion) روی سرورهای مقصد (`/companion`)
- **نسخهٔ ۰٫۲:** چک‌لیست آمادگی آفلاین برای هر برنامه، کش apt داخل هاب (پراکسی + پیش‌دریافت بسته‌ها و وابستگی‌ها)، پاسخ به سرویس‌های تشخیص IP، و نظر دوم اختیاری هوش مصنوعی

## نصب

با اینترنت (یا از روی یک هاب دیگر):

```bash
bash <(curl -Ls https://raw.githubusercontent.com/sahand363/sahand-mirror/main/install.sh)
# یا از هاب داخلی دیگر:
bash <(curl -Ls http://HUB-IP:8686/i/sahand-mirror)
```

بدون هیچ اینترنتی، از روی یک بسته:

```bash
tar -xzf sahand-mirror-bundle.tar.gz install.sh
bash install.sh --offline sahand-mirror-bundle.tar.gz
```

نصب‌کننده پورت را می‌پرسد (پیش‌فرض 8686)، سرویس را راه می‌اندازد، آدرس پنل و **رمز تصادفی ادمین** را چاپ می‌کند و منو را باز می‌کند. صفحهٔ «اولین بازدیدکننده = ادمین» وجود ندارد؛ رمز را فقط از خروجی نصب یا منو (گزینهٔ ۳) می‌گیرید.

## دو روش پر کردن هاب

1. **مستقیم**: اگر سرور هاب الان به گیت‌هاب دسترسی دارد، هر روز در ساعت تعیین‌شده (تنظیمات) و با دکمهٔ «همگام‌سازی» خودش به‌روز می‌شود. توکن گیت‌هاب اختیاری است؛ بدون توکن از صفحات عمومی گیت‌هاب (releases.atom و expanded_assets) استفاده می‌کند که محدودیت ۶۰ درخواست در ساعت API را ندارند. اگر همگام‌سازی شکست بخورد، نوار زرد «حالت آفلاین — آخرین همگام‌سازی موفق: …» نمایش داده می‌شود؛ وقتی همه چیز درست باشد سبز است.
2. **بسته‌ای**: روی هر سیستمی که اینترنت دارد:
   ```bash
   sahand-mirror collect --apps apps.json --out bundle.tar.gz   # apps.json را از پنل هاب دانلود کنید
   ```
   بعد فایل را به سرور داخلی ببرید و `sahand-mirror import bundle.tar.gz` (یا آپلود در صفحهٔ «بسته‌ها»). هر فایل با sha256 مندرج در `manifest.json` بررسی می‌شود. بسته شامل باینری خود هاب و `install.sh` هم هست.

## افزودن برنامه

- **با مخزن**: `owner/repo` — N ریلیز آخر (پیش‌فرض ۳) با همهٔ فایل‌ها + سورس شاخهٔ اصلی + سورس هر تگ. دستور نصب از README/install.sh پیدا می‌شود.
- **با دستور نصب**: مثلاً `bash <(curl -Ls https://raw.githubusercontent.com/mhsanaei/3x-ui/master/install.sh)`. هاب اسکریپت را دانلود و (به‌صورت بازگشتی) آدرس‌هایش را پیدا می‌کند. هر ارجاع به `github.com/OWNER/REPO/…`، `api.github.com/repos/OWNER/REPO/…` یا `raw.githubusercontent.com/OWNER/REPO/…` باعث آینه‌شدن کامل آن مخزن می‌شود؛ پس آدرس‌هایی که با متغیر ساخته می‌شوند (`${tag_version}`، `$(arch)`) هم کار می‌کنند. آدرس‌های غیرگیت‌هابی (مثل `https://get.acme.sh`) در فهرست «وابستگی‌های خارجی» با کلید روشن/خاموش می‌آیند.
- در فرم افزودن، معماری‌ها (پیش‌فرض amd64 و arm64) و تعداد ریلیز را همان اول انتخاب کنید. مخزن‌هایی که اسکریپت به آن‌ها ارجاع می‌دهد (مثل `Loyalsoldier/v2ray-rules-dat` در 3x-ui) در «تنظیمات آینه» تیک دارند؛ تیک را بردارید تا آینه نشوند. مخزن‌هایی که گیت‌هاب به نام جدید هدایت می‌کند (rename/transfer) فقط یک‌بار و با نام اصلی ذخیره می‌شوند.
- برای هر برنامه می‌توانید معماری‌ها را انتخاب کنید (مثلاً فقط amd64 و arm64؛ ریلیز 3x-ui برای هر معماری ~۷۵ مگابایت است)، تعداد ریلیز، regex حذف فایل و پیش‌انتشارها. فایل‌های APK همیشه نگه داشته می‌شوند و تگ‌های جدا مثل `android-v…` کنار `v…` حفظ می‌شوند.
- **رسپی**: اسکریپت نصب را در پنل ویرایش کنید؛ دستور نصب نسخهٔ ویرایش‌شده را اجرا می‌کند.

## چک‌لیست آمادگی آفلاین (بدون AI)

بعد از هر همگام‌سازی، هاب همهٔ اسکریپت‌های نصب برنامه را (به‌صورت بازگشتی، مثلاً install.sh و x-ui.sh) ذخیره و تحلیل می‌کند و در صفحهٔ برنامه یک چک‌لیست می‌سازد (خلاصه‌اش در داشبورد):

| دسته | مثال در 3x-ui | وضعیت | اقدام یک‌کلیکی |
|---|---|---|---|
| بسته‌های سیستم‌عامل (apt/dnf/yum/apk/pacman/zypper، با نام بسته‌ها) | `cron curl tar tzdata socat ca-certificates openssl` | apt: «آماده» وقتی برای همهٔ هدف‌ها در کش است؛ بقیه: آفلاین ممکن نیست | «پیش‌دریافت بسته‌ها» |
| سرویس‌های تشخیص IP | `api4.ipify.org`، `ipv4.icanhazip.com` … | آماده (به ‎/myip‎ هدایت می‌شود) | — |
| Let's Encrypt / acme.sh / certbot | `~/.acme.sh/acme.sh --issue` | نیاز به اقدام | «صدور گواهی داخلی» |
| ایمیج داکر / compose | `docker compose` | آفلاین ممکن نیست | راهنما (docker save/load) |
| pip / npm / go install / cargo / gem | `pip install pyasynchat` | آفلاین ممکن نیست | راهنما |
| مخزن apt شخص ثالث | `apt.postgresql.org.sh`، packagecloud | HTTPS = روز قطعی شکست می‌خورد | راهنما |
| آدرس‌های خارجی آینه‌نشده | `https://get.acme.sh` | نیاز به اقدام / آفلاین ممکن نیست (اگر متغیر دارد) | «آینه شود» |

دکمهٔ «تحلیل دوباره» بدون اینترنت هم کار می‌کند (روی اسکریپت‌های ذخیره‌شده). برنامه‌هایی که با نسخهٔ ۰٫۱ آینه شده‌اند در اولین اجرای ۰٫۲ از روی فایل‌های آینه تحلیل می‌شوند.

## کش apt داخل هاب

هاب روی همان پورت یک **پراکسی HTTP برای apt** دارد. دستور نصب (`/i/APP`) توابع `apt-get` و `apt` را تعریف می‌کند که فقط برای همان نصب `-o Acquire::http::Proxy=http://HUB/` اضافه می‌کنند (curl/wget مثل قبل بازنویسی می‌شوند):

- تا وقتی هاب به اینترنت (یا میرور داخلی) دسترسی دارد، فایل‌ها را از مخزن اصلی می‌گیرد و کش می‌کند: ‎.deb‎ و فایل‌های by-hash برای همیشه، فهرست‌ها (InRelease، Packages) با به‌روزرسانی. وقتی مخزن اصلی در دسترس نیست، از کش جواب می‌دهد.
- کش برای آرشیوهای اصلی بر اساس «خانواده» است، نه نام میزبان: `de.archive.ubuntu.com`، `archive.ubuntu.com`، `security.ubuntu.com` و میرور داخلی‌ای که در تنظیمات می‌دهید همه یک کش مشترک دارند (همین‌طور `deb.debian.org`/`ftp.*.debian.org` و `ports.ubuntu.com`).
- **میرور داخلی** (تنظیمات ← کش apt): برای هر خانواده (ubuntu، ubuntu-ports، debian، debian-security) یک آدرس http داخل ایران بدهید؛ وقتی مخزن اصلی جواب نمی‌دهد، فایل‌های بیرون از کش از آنجا گرفته و کش می‌شوند.
- **پیش‌دریافت**: در چک‌لیست برنامه «پیش‌دریافت بسته‌ها» را بزنید. هاب Packages.gz را از میرورهای تنظیم‌شده می‌خواند، Depends و Pre-Depends را حل می‌کند (اولین گزینهٔ موجود؛ بالاترین نسخه بین suite، ‎-updates‎ و ‎-security‎) و همهٔ ‎.deb‎ها و فهرست‌ها را برای هدف‌های انتخابی کش می‌کند: Ubuntu jammy/noble و Debian bookworm/trixie (در تنظیمات قابل انتخاب) × معماری‌های برنامه (amd64/arm64/armhf). نمونه: `socat cron` برای noble/amd64 ≈ ۵۳ فایل، ۱۸ مگابایت (+ فهرست‌های main و universe حدود ۶۵ مگابایت).
- **مخزن‌های HTTPS** (docker، cloudsmith، packagecloud …) از پراکسی رد نمی‌شوند و کش‌شدنی نیستند. دستور نصب قبل از اولین apt-get میزبان‌های https در sources.list را با یک اتصال ۴ ثانیه‌ای امتحان می‌کند؛ آن‌هایی که در دسترس نیستند فقط برای همین نصب کنار گذاشته می‌شوند (یک کپی فیلترشده از sources با `Dir::Etc::SourceList`، فایل‌های سیستم دست نمی‌خورند). اگر `apt-get update` باز هم خطا بدهد، نصب با فهرست‌های موجود ادامه پیدا می‌کند. `?apt_https=keep` این فیلتر را خاموش و `?apt=0` کل پراکسی apt را برای یک نصب خاموش می‌کند.
- **استفادهٔ دائمی یک سرور** از کش هاب (منوی `sahand-mirror` گزینهٔ ۱۱، یا تنظیمات و داشبورد):
  ```bash
  echo 'Acquire::http::Proxy "http://10.x.x.x:8686/";' > /etc/apt/apt.conf.d/99sahand-mirror && apt-get update
  # برگرداندن: rm /etc/apt/apt.conf.d/99sahand-mirror
  ```
  اگر «کلید دسترسی» فعال است آدرس به شکل `http://sm:KEY@10.x.x.x:8686/` است. راه دیگر: در sources.list آدرس میرور را با `http://10.x.x.x:8686/apt/archive.ubuntu.com/ubuntu` جایگزین کنید.
- کش apt و اسکریپت‌های ذخیره‌شده داخل بستهٔ خروجی (export/collect) هم هستند.

## تشخیص IP

`http://HUB/myip` آدرس IP درخواست‌دهنده را به‌صورت متن برمی‌گرداند (`?format=json` ← `{"ip":"…"}`). دستور نصب این آدرس‌ها را (با یا بدون https://) به ‎/myip‎ هدایت می‌کند: `api.ipify.org` `api4.ipify.org` `api64.ipify.org` `icanhazip.com` `ipv4.icanhazip.com` `ident.me` `4.ident.me` `checkip.amazonaws.com` `ifconfig.me` `ifconfig.co` `ip.sb` `ipinfo.io/ip` `v4.api.ipinfo.io/ip` `ipv4.myexternalip.com/raw` `check-host.net/ip` `ipecho.net/plain` و چند مورد دیگر. برای میزبان‌هایی که مسیرهای دیگری هم دارند (مثلاً `ipinfo.io/json`) فقط مسیر IP خالص هدایت می‌شود. هدر X-Forwarded-For فقط وقتی در تنظیمات روشن کنید (هاب پشت reverse proxy) استفاده می‌شود. نتیجه، IP سرور مقصد از دید هاب است (یعنی IP داخلی/ملی).

## هوش مصنوعی (اختیاری)

در تنظیمات می‌توانید Gemini (کلید API) یا هر سرویس سازگار با OpenAI (base URL + کلید + مدل) بدهید؛ کلید فقط‌نوشتنی است. دکمهٔ «نظر AI» در صفحهٔ برنامه اسکریپت‌ها (کوتاه‌شده) و یافته‌های فعلی را می‌فرستد و موارد اضافه (وابستگی‌ها، ریسک‌ها، پیشنهادها) را با برچسب AI به چک‌لیست اضافه می‌کند. فقط با اینترنت کار می‌کند و بدون آن خطای روشن می‌دهد؛ هیچ بخشی از هاب به آن وابسته نیست.

## Companion روی سرورهای مقصد (نسخهٔ ۰٫۳)

دستور نصب در داشبورد هاب (کارت «نصب Companion») آمادهٔ کپی است و آدرس خود هاب داخلش هست:

```bash
bash <(curl -Ls http://10.x.x.x:8686/companion)
```

Companion یک منوی SSH (`smc`) و یک پنل وب می‌دهد: شبیه‌ساز اینترنت ملی (همه‌چیز جز هاب قطع، با برگشت خودکار)، تست دسترسی به سایت‌های معروف، و فهرست برنامه‌های هاب با تشخیص «جدید / به‌روز شد / حذف شد»، دستور نصب و README. برنامهٔ `sahand-mirror-companion` از این نسخه یک‌بار خودکار به فهرست برنامه‌ها اضافه می‌شود (اگر حذفش کنید دوباره اضافه نمی‌شود).

API عمومی (با همان کلید دسترسی اگر فعال باشد):

- `GET /catalog`: برنامه‌ها با نسخهٔ آخر، تعداد و حجم فایل‌ها، دستور نصب از هاب، خلاصهٔ آمادگی آفلاین و وجود README
- `GET /catalog/<id>` و `GET /catalog/<id>/readme`
- `GET /companion`: همان `/i/sahand-mirror-companion`

## تست قطعی روی یک سرور مقصد

ساده‌ترین راه: Companion را نصب کنید و در منو گزینهٔ ۴ یا کلید بالای پنل را بزنید. روش دستی:

برای اینکه ببینید نصب واقعاً فقط با هاب جلو می‌رود، روی **سرور مقصد** (نه هاب) همهٔ ترافیک خروجی جز هاب را ببندید؛ با برگشت خودکار بعد از ۲۰ دقیقه (حتی اگر SSH قطع شود):

```bash
HUB_IP=10.x.x.x HUB_PORT=8686
iptables -N SM_TEST
iptables -A SM_TEST -o lo -j ACCEPT
iptables -A SM_TEST -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT   # جلسهٔ SSH فعلی
iptables -A SM_TEST -d "$HUB_IP" -p tcp --dport "$HUB_PORT" -j ACCEPT
iptables -A SM_TEST -j REJECT
iptables -I OUTPUT 1 -j SM_TEST
ip6tables -I OUTPUT 1 ! -o lo -j REJECT
# برگشت خودکار
systemd-run --on-active=20m --unit=sm-rollback /bin/sh -c \
  'iptables -D OUTPUT -j SM_TEST; iptables -F SM_TEST; iptables -X SM_TEST; ip6tables -D OUTPUT ! -o lo -j REJECT'
curl -s --max-time 5 https://github.com >/dev/null && echo "github هنوز باز است!" || echo "قطع شد؛ فقط هاب در دسترس است"
bash <(curl -Ls "http://$HUB_IP:$HUB_PORT/i/3x-ui?trace=1")
# پایان زودتر: systemctl start sm-rollback   (یا همان سه دستور داخل systemd-run)
```

- REJECT خطای فوری می‌دهد؛ در قطعی واقعی معمولاً بسته‌ها DROP می‌شوند و هر تلاش تا timeout صبر می‌کند، پس نصب واقعی ممکن است کندتر باشد. DNS هم در این تست بسته است (در قطعی واقعی DNS داخلی معمولاً کار می‌کند)؛ چون همه‌چیز با IP هاب است مشکلی ایجاد نمی‌کند.
- **سرعت:** اگر سرور تست خارج از ایران است و هاب داخل ایران، دانلودها از مسیر بین‌الملل می‌آیند و کند هستند (مثلاً ریلیز ۷۵ مگابایتی 3x-ui). این طبیعی است؛ روز قطعی، سرور داخلی ← هاب داخلی روی شبکهٔ ملی سریع است.

## روز قطعی — قدم‌به‌قدم

1. پنل را با IP داخلی باز کنید: `http://10.x.x.x:8686` (آدرس در منوی `sahand-mirror` گزینهٔ ۲ هم هست).
2. «برنامه‌ها» ← برنامه را انتخاب کنید ← در صورت نیاز نسخه را عوض کنید ← «کپی».
3. روی سرور مقصد (با root) اجرا کنید:
   ```bash
   bash <(curl -Ls http://10.x.x.x:8686/i/3x-ui)
   ```
   این دستور توابع `curl` و `wget` (و برای پردازه‌های فرزند مثل `sh`، فایل‌های جایگزین در PATH) تعریف می‌کند که هر آدرس github.com / api.github.com / raw.githubusercontent.com / objects.githubusercontent.com و وابستگی‌های خارجی آینه‌شده را به هاب می‌فرستند، `MIRROR_URL` را export می‌کند، apt-get/apt را از کش apt هاب عبور می‌دهد، سرویس‌های تشخیص IP را به ‎/myip‎ می‌فرستد و بعد همان دستور نصب اصلی را اجرا می‌کند. در پرسش‌های SSL گزینهٔ self-signed یا رد کردن را بزنید (قدم ۴). پارامترها: `?v=v2.4.0` (قفل نسخه)، `?args=...` (آرگومان اسکریپت)، `?NAME=value` (متغیر محیطی)، `?trace=1` (نمایش هدایت‌ها).
4. **SSL بدون Let's Encrypt**: صفحهٔ «گواهی‌ها» ← IP یا دامنهٔ سرور را بدهید ← zip شامل `fullchain.pem` و `privkey.pem` دانلود می‌شود؛ در پنل برنامه (مثلاً 3x-ui) مسیر این دو را وارد کنید. گواهی ریشه (`/ca.crt`) را یک‌بار روی کلاینت‌ها و گوشی‌ها نصب کنید:
   ```bash
   curl -s http://10.x.x.x:8686/ca.crt -o /usr/local/share/ca-certificates/sahand-mirror.crt && update-ca-certificates
   ```
5. **گوشی‌ها**: صفحهٔ عمومی `http://10.x.x.x:8686/d?apk=1` همهٔ APKها را نشان می‌دهد؛ مستقیم دانلود و نصب کنید.
6. **اگر هاب از بین رفت**: از قبل هر از گاهی «بسته‌ها ← دانلود بسته» یا `sahand-mirror export --out …` بگیرید و جای دیگری نگه دارید. روی سرور جدید: `bash install.sh --offline bundle.tar.gz` — بدون هیچ اینترنتی هاب کامل برمی‌گردد.

## امنیت

رمز ادمین با PBKDF2-SHA256 ذخیره می‌شود؛ محدودیت ورود (۵ خطا = ۵ دقیقه قفل)؛ کوکی HttpOnly و SameSite=Strict (و Secure در HTTPS)؛ هدر ضد CSRF برای API؛ سقف حجم آپلود؛ محافظت در برابر path traversal در همهٔ مسیرهای فایل؛ نوشتن اتمیک فایل‌ها. «کلید دسترسی» اختیاری در تنظیمات، مسیرهای عمومی `/i`، `/gh`، `/ext`، `/d`، `/myip`، `/apt` و پراکسی apt را محافظت می‌کند (پراکسی: `http://sm:KEY@HUB/`) (`?k=` یا هدر `X-Mirror-Key`؛ دستور نصب کلید را خودش اضافه می‌کند). پراکسی apt پراکسی باز نیست: فقط GET/HEAD برای مسیرهای مخزن apt (`dists/`، `pool/`، ‎*.deb‎) روی http؛ CONNECT/https رد می‌شود. هاب به‌طور پیش‌فرض HTTP است؛ گزینهٔ HTTPS با گواهی CA داخلی در تنظیمات هست.

## منو (`sahand-mirror` با root)

وضعیت · آدرس پنل و ورود · ریست رمز · تغییر پورت · همگام‌سازی · خروجی بسته · ورود بسته · لاگ‌ها · به‌روزرسانی (از گیت‌هاب یا از یک هاب) · حذف · دستور استفادهٔ دائمی از کش apt. دستورات CLI: `serve`، `init`، `reset-password`، `status`، `sync`، `export`، `import`، `collect`.

## محدودیت‌های شناخته‌شده

- **apt**: فقط مخزن‌های http کش می‌شوند و فقط apt (اوبونتو/دبیان)؛ dnf/yum/apk/pacman/zypper به میرور داخلی سیستم‌عامل نیاز دارند. بسته‌ای که نه پیش‌دریافت شده و نه یک‌بار از هاب نصب شده، روز قطعی فقط از «میرور داخلی» تنظیم‌شده قابل دریافت است. پیش‌دریافت نسخه‌ها را دقیق مقایسه نمی‌کند (اولین گزینهٔ هر وابستگی، بدون بررسی محدودیت نسخه) و بسته‌هایی که سرور مقصد از قبل دارد را هم دانلود می‌کند. اگر هاب HTTPS باشد، پراکسی apt در دستور نصب استفاده نمی‌شود (از مسیر `/apt/<host>/…` استفاده کنید).
- تحلیلگر بر اساس الگو کار می‌کند: بسته‌هایی که با متغیر ساخته می‌شوند (`${PKGS}`) را «نیاز به اقدام» نشان می‌دهد و مدیر بسته‌ای که خودش در متغیر است (`$PM install`) را نمی‌بیند؛ برای این موارد «بسته‌های اضافه» یا «نظر AI» را استفاده کنید. همهٔ شاخه‌های اسکریپت (مثلاً postgresql اختیاری 3x-ui) فهرست می‌شوند، نه فقط شاخه‌ای که واقعاً اجرا می‌شود.
- آدرس‌هایی که فقط با متغیر ساخته می‌شوند و روی گیت‌هاب نیستند (`https://${domain}/…`) قابل آینه نیستند.
- `sudo` محیط را پاک می‌کند؛ دستور نصب را مستقیم با root اجرا کنید، نه `sudo bash <(…)`. ابزارهایی که curl را با مسیر کامل (`/usr/bin/curl`) صدا می‌زنند یا از python/Go مستقیم به گیت‌هاب وصل می‌شوند هدایت نمی‌شوند.
- `raw.githubusercontent.com/…/<ref>/…` فقط برای شاخهٔ اصلی، تگ‌های آینه‌شده و فایل‌هایی که صریحاً در اسکریپت آمده‌اند جواب می‌دهد؛ نام شاخهٔ دیگر (مثل master به‌جای main) به شاخهٔ اصلی برمی‌گردد.
- مخزن‌های خصوصی فقط با توکن گیت‌هابی که به آن‌ها دسترسی دارد آینه می‌شوند.

---

## English (short)

**sahand-mirror** is a single-binary Go hub (stdlib only, embedded offline UI in Persian) that mirrors GitHub-installed apps onto an internal server, so that during an international internet shutdown any internal server can still install them.

- Add an app by `owner/repo` or by its one-line install command. The hub scans the install script (recursively) and mirrors the newest N releases (all assets, arch-filterable), the default-branch and tag source tarballs, literal raw files, and optionally non-GitHub static files.
- It emulates GitHub: `/gh/raw/…`, `/gh/api/repos/…/releases[/latest|/tags/T]`, `/gh/web/…/releases/latest` (302 to the tag, so `url_effective` tricks work), `/gh/web/…/releases/download/T/A` (+ `.sha256`), `/gh/web/…/archive/…tar.gz`, `/ext?u=`.
- `bash <(curl -Ls http://HUB:8686/i/APP)` defines exported `curl`/`wget` functions plus PATH shims that rewrite GitHub URLs to the hub, exports `MIRROR_URL`, then runs the app's original (or edited "recipe") install command unmodified. `?v=TAG` pins a version.
- Sync directly from GitHub daily (token optional; falls back to non-API web pages), or offline: `sahand-mirror collect --out bundle.tar.gz` on any internet machine, then `import`. Bundles carry a sha256 manifest, the hub binary and `install.sh`, so `bash install.sh --offline bundle.tar.gz` installs a fresh hub with zero internet.
- Internal CA: download `/ca.crt`, issue cert+key zips for IPs/domains instead of Let's Encrypt.
- v0.2: per-app offline-readiness checklist (deterministic script analysis: OS packages per package manager, IP-detection services, Let's Encrypt/acme.sh/certbot, docker images, pip/npm/go, third-party apt repos, unmirrored URLs) with one-click fixes; an apt caching forward proxy on the same port (`Acquire::http::Proxy=http://HUB/`, host-independent cache for Ubuntu/Debian archives, internal-mirror fallback, offline serve) plus a prefetch job that resolves Depends/Pre-Depends from Packages.gz for jammy/noble/bookworm/trixie × app archs; `/myip` (IP-detection hosts are rewritten to it); optional AI second opinion (Gemini or OpenAI-compatible); per-repo enable/disable; redirected repos stored once.
- Limits: only http apt repos are cached (https third-party repos are skipped for the install when unreachable); rpm/apk/pacman need an internal OS mirror; `sudo` strips the wrapper environment.

Build: `go build .` · Test: `go vet ./... && go test ./...` · Release: push a `v*` tag (GitHub Actions builds linux amd64/arm64/armv7 tar.gz + sha256).
