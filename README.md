# سهند میرور (sahand-mirror)

هاب داخلی برای روزهای قطعی اینترنت بین‌الملل. برنامه‌هایی که معمولاً از گیت‌هاب نصب می‌شوند (3x-ui، zimzim، loadinme، easy-xui، blendartai و …) را — با ریلیزها، اسکریپت نصب، سورس و فایل‌های لازم — روی یک سرور داخل ایران آینه می‌کند تا وقتی فقط اینترنت ملی کار می‌کند، روی هر سرور دیگری با یک خط نصبشان کنید. اسکریپت‌های نصب **بدون تغییر** اجرا می‌شوند: هاب خودش نقش github.com، api.github.com و raw.githubusercontent.com را بازی می‌کند.

- یک فایل اجرایی Go (فقط کتابخانهٔ استاندارد)، پنل وب فارسی و راست‌به‌چپ، بدون CDN و فونت خارجی
- تنظیمات: `/etc/sahand-mirror/config.json` · داده‌ها: `/var/lib/sahand-mirror` · باینری: `/usr/local/bin/sahand-mirror`
- سرویس systemd + منوی مدیریتی (`sahand-mirror` با دسترسی root)

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
- برای هر برنامه می‌توانید معماری‌ها را انتخاب کنید (مثلاً فقط amd64 و arm64؛ ریلیز 3x-ui برای هر معماری ~۷۵ مگابایت است)، تعداد ریلیز، regex حذف فایل و پیش‌انتشارها. فایل‌های APK همیشه نگه داشته می‌شوند و تگ‌های جدا مثل `android-v…` کنار `v…` حفظ می‌شوند.
- **رسپی**: اسکریپت نصب را در پنل ویرایش کنید؛ دستور نصب نسخهٔ ویرایش‌شده را اجرا می‌کند.

## روز قطعی — قدم‌به‌قدم

1. پنل را با IP داخلی باز کنید: `http://10.x.x.x:8686` (آدرس در منوی `sahand-mirror` گزینهٔ ۲ هم هست).
2. «برنامه‌ها» ← برنامه را انتخاب کنید ← در صورت نیاز نسخه را عوض کنید ← «کپی».
3. روی سرور مقصد (با root) اجرا کنید:
   ```bash
   bash <(curl -Ls http://10.x.x.x:8686/i/3x-ui)
   ```
   این دستور توابع `curl` و `wget` (و برای پردازه‌های فرزند مثل `sh`، فایل‌های جایگزین در PATH) تعریف می‌کند که هر آدرس github.com / api.github.com / raw.githubusercontent.com / objects.githubusercontent.com و وابستگی‌های خارجی آینه‌شده را به هاب می‌فرستند، `MIRROR_URL` را export می‌کند و بعد همان دستور نصب اصلی را اجرا می‌کند. پارامترها: `?v=v2.4.0` (قفل نسخه)، `?args=...` (آرگومان اسکریپت)، `?NAME=value` (متغیر محیطی)، `?trace=1` (نمایش هدایت‌ها).
4. **SSL بدون Let's Encrypt**: صفحهٔ «گواهی‌ها» ← IP یا دامنهٔ سرور را بدهید ← zip شامل `fullchain.pem` و `privkey.pem` دانلود می‌شود؛ در پنل برنامه (مثلاً 3x-ui) مسیر این دو را وارد کنید. گواهی ریشه (`/ca.crt`) را یک‌بار روی کلاینت‌ها و گوشی‌ها نصب کنید:
   ```bash
   curl -s http://10.x.x.x:8686/ca.crt -o /usr/local/share/ca-certificates/sahand-mirror.crt && update-ca-certificates
   ```
5. **گوشی‌ها**: صفحهٔ عمومی `http://10.x.x.x:8686/d?apk=1` همهٔ APKها را نشان می‌دهد؛ مستقیم دانلود و نصب کنید.
6. **اگر هاب از بین رفت**: از قبل هر از گاهی «بسته‌ها ← دانلود بسته» یا `sahand-mirror export --out …` بگیرید و جای دیگری نگه دارید. روی سرور جدید: `bash install.sh --offline bundle.tar.gz` — بدون هیچ اینترنتی هاب کامل برمی‌گردد.

## امنیت

رمز ادمین با PBKDF2-SHA256 ذخیره می‌شود؛ محدودیت ورود (۵ خطا = ۵ دقیقه قفل)؛ کوکی HttpOnly و SameSite=Strict (و Secure در HTTPS)؛ هدر ضد CSRF برای API؛ سقف حجم آپلود؛ محافظت در برابر path traversal در همهٔ مسیرهای فایل؛ نوشتن اتمیک فایل‌ها. «کلید دسترسی» اختیاری در تنظیمات، مسیرهای عمومی `/i`، `/gh`، `/ext`، `/d` را محافظت می‌کند (`?k=` یا هدر `X-Mirror-Key`؛ دستور نصب کلید را خودش اضافه می‌کند). هاب به‌طور پیش‌فرض HTTP است؛ گزینهٔ HTTPS با گواهی CA داخلی در تنظیمات هست.

## منو (`sahand-mirror` با root)

وضعیت · آدرس پنل و ورود · ریست رمز · تغییر پورت · همگام‌سازی · خروجی بسته · ورود بسته · لاگ‌ها · به‌روزرسانی (از گیت‌هاب یا از یک هاب) · حذف. دستورات CLI: `serve`، `init`، `reset-password`، `status`، `sync`، `export`، `import`، `collect`.

## محدودیت‌های شناخته‌شده

- **بسته‌های apt/dnf سیستم‌عامل** آینه نمی‌شوند؛ اگر اسکریپت `apt-get install` می‌زند، یک میرور داخلی سیستم‌عامل لازم است (مثلاً میرورهای داخلی دیتاسنتر). در تنظیمات می‌توانید «راهنمای میرور apt» بگذارید تا در خروجی نصب نمایش داده شود. پیشنهاد: پیش‌نیازهایی مثل curl, tar, socat را از قبل روی سرورها نصب کنید.
- **سرویس‌های تشخیص IP** (ipify، icanhazip …) قابل آینه‌کردن نیستند؛ اسکریپت‌هایی که به آن‌ها وابسته‌اند ممکن است IP عمومی را پیدا نکنند (3x-ui در این حالت ادامه می‌دهد).
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
- Limits: OS packages need an internal OS mirror; IP-detection services can't be mirrored; `sudo` strips the wrapper environment.

Build: `go build .` · Test: `go vet ./... && go test ./...` · Release: push a `v*` tag (GitHub Actions builds linux amd64/arm64/armv7 tar.gz + sha256).
