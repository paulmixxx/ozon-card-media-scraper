# ozon-card-media-scraper

CLI-утилита для Linux, которая по ссылке на товар Ozon:

- создает директорию `<productSlug>` или `<productSlug>_yyyymmdd`, если такая уже существует;
- внутри создает папки `card/` и `review/`;
- скачивает медиа карточки товара (картинки/видео), если они доступны в HTML/embedded JSON;
- пытается получить отзывы и их медиа через Ozon widget API;
- сохраняет метаданные в `product.json`, `reviews.json` и, при ошибке антибота, в `error.json`.

## Почему есть предупреждение про anti-bot

Ozon активно режет запросы из серверных/VPS сетей. В таком случае бинарник вернет понятную ошибку про `anti-bot blocked request`.

Для реального запуска на стороне пользователя обычно помогают:

- домашний / мобильный IP;
- `--cookie-file cookies.txt` с Cookie из живой браузерной сессии Ozon;
- `--proxy` с доверенным residential/mobile proxy.

## Сборка

```bash
go build -o dist/ozon-card-media-scraper .
```

## Использование

```bash
./dist/ozon-card-media-scraper \
  --output ./downloads \
  --reviews-pages 20 \
  --cookie-file ./cookies.txt \
  'https://www.ozon.ru/product/sharf-snud-essentials-696919507/'
```

## Аргументы

- `--output` — базовая папка выгрузки, по умолчанию `.`
- `--reviews-pages` — сколько страниц отзывов максимум запрашивать
- `--cookie` — raw значение заголовка `Cookie`
- `--cookie-file` — путь к файлу с Cookie
- `--proxy` — HTTP(S) proxy URL
- `--timeout` — timeout запроса в секундах
- `--write-html` — сохранить HTML карточки в `product.html`
- `--user-agent` — кастомный User-Agent

## Структура результата

```text
<productSlug>/
  card/
  review/
    <review-id>/
  product.json
  reviews.json
```

## Проверка

Локальные тесты:

```bash
go test ./...
```

Сборка бинарника:

```bash
go build -o dist/ozon-card-media-scraper .
```
