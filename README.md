# ozon-card-media-scraper

CLI-утилита для Linux, которая по ссылке на товар Ozon:

- создает директорию `<productSlug>` или `<productSlug>_yyyymmdd`, если такая уже существует;
- внутри создает папки `card/` и `review/`;
- скачивает медиа карточки товара (картинки/видео);
- скачивает медиа отзывов;
- сохраняет метаданные в `product.json`, `reviews.json` и, при ошибке антибота, в `error.json`.

## Режимы работы

### 1) HTTP-режим

Быстрый режим без браузера. Подходит, когда Ozon не включает жесткий anti-bot.

### 2) Browser mode

Режим через **реальный Chromium**. Нужен, когда Ozon делает редирект вида `?__rr=1&abt_att=1` и обычный HTTP-клиент не проходит защиту.

Именно этот режим рекомендуется для запуска **на том же ПК**, где страница открывается в браузере.

## Почему есть предупреждение про anti-bot

Ozon активно режет запросы из серверных/VPS сетей. В таком случае бинарник вернет понятную ошибку про `anti-bot blocked request`.

Для реального запуска на стороне пользователя обычно помогают:

- домашний / мобильный IP;
- `--cookie-file cookies.txt` с Cookie из живой браузерной сессии Ozon;
- `--browser-mode --no-headless` на том же ПК, где Ozon открывается в браузере;
- `--proxy` с доверенным residential/mobile proxy.

## Сборка

```bash
go build -o dist/ozon-card-media-scraper .
```

## Использование

### Обычный режим

```bash
./dist/ozon-card-media-scraper \
  --output ./downloads \
  --reviews-pages 20 \
  --cookie-file ./cookies.txt \
  'https://www.ozon.ru/product/sharf-snud-essentials-696919507/'
```

### Browser mode

```bash
./dist/ozon-card-media-scraper \
  --browser-mode \
  --no-headless \
  --browser-wait 20 \
  --output ./downloads \
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
- `--browser-mode` — включить режим через реальный Chromium
- `--browser-path` — путь к Chromium/Chrome, если нужен явный бинарник
- `--browser-wait` — сколько секунд ждать гидратации страницы в browser mode
- `--no-headless` — запускать браузер с UI; для локального ПК это рекомендуемый режим

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
