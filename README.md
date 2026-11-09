# Washing Loyalty

Go-сервис программы лояльности автомойки. PostgreSQL хранит пользователей,
балансы клиентов, историю начислений и настройки станций. `washing_connector`
отвечает за оборудование, привязку клиента к станции и текущий баланс станции.

## Поведение

- Регистрация создаёт пользователя с нулевым балансом и выдаёт Bearer-токен.
- `POST /scan` связывает пользователя со станцией через HTTP-коннектор.
  Сканирование само по себе **не меняет баланс и не создаёт историю**. Списание — отдельная подтверждаемая операция.
- Оборудование передаёт начисления через коннектор в `POST /internal/accruals`.
  Изменение баланса и запись истории выполняются одной транзакцией.
- `event_id` уникален во всей системе. Повтор события возвращает прежнюю запись
  без повторного начисления. Повтор с другими данными возвращает `409`.
- Баланс станции запрашивается у коннектора при каждом обращении и не сохраняется
  в PostgreSQL. При недоступном коннекторе баланс не подменяется нулём.
- Балансы и начисления — целые неотрицательные числа (`int64`) в минимальных
  единицах бонусов. Начисление через webhook имеет `diff > 0`; подтверждённое списание через `/redeem` или WebSocket записывается с `diff < 0`.

## Запуск

Нужны Go 1.27.3 и PostgreSQL 17 либо Podman с Compose-провайдером.

```sh
cp .env.example .env
```

Заполните в `.env` четыре **разных** секрета `AUTH_SECRET`, `ADMIN_TOKEN`,
`CONNECTOR_TOKEN`, `WEBHOOK_TOKEN` (не менее 32 байт каждый). Для каждого:

```sh
openssl rand -hex 32
```

Для локального Go-процесса укажите `CONNECTOR_URL=http://localhost:8081`.
Compose собирает коннектор из соседнего `../washing_connector` и использует
`http://connector:8081`. Для внешнего коннектора задайте `CONNECTOR_CONTAINER_URL`.
Контракт описан в [docs/connector.md](docs/connector.md). Реализован тип `mock`;
инструкции самостоятельного запуска — в `../washing_connector/README.md`.

Полный запуск в контейнерах:

```sh
podman compose up --build -d
curl http://localhost:8080/readyz
```

Запуск Go на хосте, PostgreSQL в контейнере:

```sh
podman compose up -d postgres
set -a
. ./.env
set +a
go run ./cmd/loyalty
```

`.env` не загружается Go-сервисом автоматически. В Compose `DATABASE_URL` для
API переопределён на имя контейнера `postgres`. Для `POSTGRES_PASSWORD` удобно
использовать hex-значение: специальные символы потребуют URL-кодирования в DSN.
Значение `loyalty` в примере предназначено для локальной разработки.

Миграции встроены в бинарник и применяются при старте, до открытия HTTP-порта.
Транзакционная advisory-блокировка защищает запуск нескольких экземпляров.
Миграции применяются один раз; изменения схемы добавляются новым SQL-файлом
в `internal/store/migrations`. База данных должна уже существовать.

## API

JSON в UTF-8. Для POST требуется `Content-Type: application/json`.
Неизвестные поля и тела более 16 KiB отклоняются. Время — RFC3339; в БД —
`TIMESTAMPTZ`. `user_id` обязан совпадать с пользователем из токена.

| Метод и путь | Вход | Авторизация |
| --- | --- | --- |
| `POST /register` | JSON: `login`, `hash`, `email` | — |
| `POST /login` | JSON: `login`, `hash` | — |
| `POST /scan` | JSON: `qr`, `user_id` | Токен пользователя |
| `GET /history` | Query: `user_id`, `page` | Токен пользователя |
| `GET /state` | Query: `user_id`, `qr` | Токен пользователя |
| `POST /admin/stations` | JSON: `name`, `point`, `qr`, `address`, `connection_type` | `ADMIN_TOKEN` |
| `GET /admin/stations` | Query: `page` | `ADMIN_TOKEN` |
| `GET /admin/stations/{id}/state` | ID станции | `ADMIN_TOKEN` |
| `POST /admin/users/{id}/reset-password` | JSON: `hash` | `ADMIN_TOKEN` |
| `POST /internal/accruals` | JSON: `event_id`, `user_id`, `qr`, `diff` | `WEBHOOK_TOKEN` |
| `GET /healthz` | — | — |
| `GET /readyz` | — | — |

Защищённые запросы используют `Authorization: Bearer <token>`.
Токены администратора, коннектора и пользователя не взаимозаменяемы.
`/healthz` проверяет процесс, `/readyz` — соединение с PostgreSQL.
Доступность оборудования проверяется запросом состояния станции.

### Регистрация и вход

```sh
curl -X POST http://localhost:8080/register \
  -H 'Content-Type: application/json' \
  -d '{"login":"alice","hash":"client-password-hash","email":"alice@example.com"}'

curl -X POST http://localhost:8080/login \
  -H 'Content-Type: application/json' \
  -d '{"login":"alice","hash":"client-password-hash"}'
```

Ответ (`201` при регистрации, `200` при входе):

```json
{
  "user": {
    "id": 1,
    "login": "alice",
    "email": "alice@example.com",
    "balance": 0,
    "updated_at": "2026-10-30T07:00:00Z"
  },
  "access_token": "<jwt>",
  "token_type": "Bearer",
  "expires_at": "2026-10-31T07:00:00Z"
}
```

Логин: 3–64 ASCII-символа `a-z`, `0-9`, `_`, `.`, `-`. Логин и email
нормализуются в нижний регистр и уникальны. `hash`: 8–1024 байта; это секрет
аутентификации клиента, даже если он уже является хешем пароля. В БД записывается
только `bcrypt(base64(SHA-256(hash)))` со случайной солью; hash не выдаётся API.

Новые токены — JWT HS256 с ролью `user` или `admin`; старые двухчастные пользовательские токены принимаются до истечения срока. Клиент хранит их как непрозрачное значение, а браузер получает HttpOnly-cookie `washing_session`.
Срок жизни по умолчанию 24 часа. Для обновления нужно снова войти.
Сброс пароля меняет соль и немедленно делает старые токены недействительными
для следующих запросов.

### Станции, сканирование и состояние

```sh
curl -X POST http://localhost:8080/admin/stations \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"name":"Мойка №1","point":"post-1","qr":"station-qr","address":"10.0.0.5:9000","connection_type":"tcp"}'

curl -X POST http://localhost:8080/scan \
  -H "Authorization: Bearer $USER_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"qr":"station-qr","user_id":1}'

curl 'http://localhost:8080/state?user_id=1&qr=station-qr' \
  -H "Authorization: Bearer $USER_TOKEN"
```

`USER_TOKEN` — `access_token` из регистрации или входа; ID берётся из `user.id`.
`point` — строковый идентификатор поста/точки, `address` — адрес, понятный
коннектору. `connection_type` — код адаптера коннектора (например `tcp`, `http`,
`modbus_tcp`), до 64 символов `a-z0-9_-`; сервис не навязывает протокол оборудования.
Лимиты в байтах: `name` и `point` — 200; `qr` и `address` — 512.

Создание станции возвращает `201` и все её поля вместе с `id`.
`/scan` возвращает `user_id`, текущий `balance` пользователя, объект `station`
и `state: {online, balance, user_id}` с подтверждённой привязкой от коннектора.
Повторное сканирование тем же клиентом должно быть идемпотентным у коннектора;
занятая или недоступная для привязки станция возвращает `409`.

Ответ `/state`:

```json
{
  "user_id": 1,
  "balance": 15,
  "station": {
    "id": 1,
    "name": "Мойка №1",
    "point": "post-1",
    "qr": "station-qr",
    "address": "10.0.0.5:9000",
    "connection_type": "tcp"
  },
  "state": {"online": true, "balance": 170, "linked_to_user": true}
}
```

Внешний `balance` принадлежит пользователю, `state.balance` — станции.
ID другого клиента не раскрывается пользовательской ручкой `/state`.

`GET /admin/stations/{id}/state` возвращает `{station, state}`, где `state`
содержит `online`, `balance`, `user_id` (или `null`). `GET /admin/stations`
возвращает такие же элементы в пагинированном `items`. При ошибке отдельной
станции её `state` равен `null`, добавляется `error: "connector_unavailable"`;
остальные станции остаются в ответе. Для прямого запроса состояния ошибка
коннектора даёт `502`.

### История и начисления

```sh
curl -X POST http://localhost:8080/internal/accruals \
  -H "Authorization: Bearer $WEBHOOK_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"event_id":"station-1-wash-0001","user_id":1,"qr":"station-qr","diff":15}'

curl 'http://localhost:8080/history?user_id=1&page=1' \
  -H "Authorization: Bearer $USER_TOKEN"
```

Новое начисление возвращает `201`, повтор — `200`:

```json
{
  "history": {
    "id": 1,
    "user_id": 1,
    "station_id": 1,
    "event_id": "station-1-wash-0001",
    "balance_after": 15,
    "diff": 15,
    "datetime": "2026-10-30T07:00:00Z"
  },
  "duplicate": false
}
```

При повторе `balance_after` — исторический баланс после исходного события.
Текущий баланс можно получить через `/state`. `event_id` ограничен 128 байтами.
`datetime` задаётся сервером в момент записи.

История: `{items: [...], page: 1, page_size: 20, has_more: false}`.
Сортировка — сначала новые записи (`datetime DESC, id DESC`). Страницы начинаются
с 1, по умолчанию 1, максимум 1 000 000. У истории и списка станций фиксированный
размер страницы 20. При новых начислениях между запросами offset-пагинация может
сместиться; это не снимок истории на один момент времени.

### Сброс пароля

```sh
curl -X POST http://localhost:8080/admin/users/1/reset-password \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"hash":"new-client-password-hash"}'
```

Ответ `204`, без тела. Администратор передаёт новое значение `hash`;
отправка писем и самостоятельное восстановление через email не входят в API.

### Ошибки

```json
{"error":{"code":"forbidden","message":"user_id does not match the authenticated user"}}
```

Основные статусы: `400` — некорректные данные, `401` — неверная авторизация,
`403` — чужой пользователь, `404` — объект не найден, `409` — дубликат/конфликт,
`415` — неподдерживаемый Content-Type, `429` — заняты слоты обработки паролей,
`502` — ошибка коннектора, `503` — БД не готова.

## Схема PostgreSQL

- `"user" (id, login, hash, email, balance, updated_at)`; имя заключено в кавычки,
  поскольку `user` — специальное слово PostgreSQL.
- `history (id, user_id, station_id, event_id, balance_after, diff, datetime)`.
  Дополнительные `id`, `station_id`, `event_id` обеспечивают порядок, связь со
  станцией и защиту от повторной доставки событий.
- `station (id, name, point, qr, address, connection_type)`; QR уникален.
- `schema_migrations` — техническая таблица версий миграций.

В таблице `station` нет баланса или текущего пользователя. Источником привязки
и состояния является коннектор. Строка пользователя блокируется при начислении;
проверка переполнения, обновление баланса и история выполняются атомарно.

## Конфигурация

| Переменная | Назначение / значение по умолчанию |
| --- | --- |
| `HTTP_ADDR` | `:8080` |
| `DATABASE_URL` | Обязательная PostgreSQL DSN |
| `AUTH_SECRET` | Секрет подписи токенов пользователей |
| `ADMIN_TOKEN` | Отдельный токен администратора |
| `CONNECTOR_URL` | Обязательный HTTP(S)-адрес коннектора |
| `CONNECTOR_TOKEN` | Bearer-токен исходящих запросов к коннектору |
| `WEBHOOK_TOKEN` | Bearer-токен входящих начислений от коннектора |
| `TOKEN_TTL` | `24h`, от `1s` до `168h` |
| `CONNECTOR_TIMEOUT` | `5s`, от `1s` до `30s` |

В production используйте TLS на reverse proxy, ограничьте доступ к
`/admin/*` и `/internal/*` и настройте лимиты частоты входа/регистрации.
Сам сервис ограничивает число одновременно выполняемых bcrypt-операций.
Для PostgreSQL вне локальной сети настройте проверку TLS через DSN.
Compose публикует порты только на `127.0.0.1`.

## Проверки и сборка

```sh
make test
make vet
make build
```

Для интеграционных тестов нужна отдельная тестовая БД. Каждый тест создаёт и
удаляет собственную схему; роль должна иметь право `CREATE SCHEMA`:

```sh
TEST_DATABASE_URL='postgres://loyalty:loyalty@localhost:5432/loyalty_test?sslmode=disable' make integration
```

Без `TEST_DATABASE_URL` тесты PostgreSQL пропускаются. В CI переменная всегда
задана. Проверяются API целиком, миграции, одновременные начисления, дедупликация,
переполнение баланса, авторизация, сброс пароля и ошибки HTTP-коннектора.

```sh
make image
podman run --rm --env-file .env -p 127.0.0.1:8080:8080 washing-loyalty:local
```

При `podman run` переменные `DATABASE_URL` и `CONNECTOR_URL` должны указывать
адреса, доступные из контейнера. Образ запускается от UID 65532, без shell и
без root, содержит статический Go-бинарник и CA-сертификаты.

## GitHub Actions / GHCR

[Workflow](.github/workflows/ci.yml) на pull request, push в `main`/`master`,
тег `v*` и ручном запуске:

1. Проверяет форматирование, зависимости, `go vet`, тесты с `-race` и PostgreSQL 17.
2. Собирает OCI-образ командой `podman build --format oci`.
3. На push/ручном запуске публикует его в `ghcr.io/<owner>/<repository>`.
   На PR выполняется сборка без публикации.

Теги: `sha-<полный commit SHA>` для каждого опубликованного образа,
`latest` для основной ветки репозитория, `v1.0.0` для одноимённого Git-тега.
Для этого репозитория имя образа: `ghcr.io/killbane1232/washing_loyalty`.
Публикация использует встроенный `GITHUB_TOKEN` с `packages: write`; отдельный
registry secret не нужен. Политика организации/пакета должна разрешать запись
этому репозиторию. Workflow начнёт выполняться после отправки файлов на GitHub.

Справка: [Podman build](https://docs.podman.io/en/latest/markdown/podman-build.1.html),
[публикация образов в GitHub Packages](https://docs.github.com/en/actions/tutorials/publish-packages/publish-docker-images).


## Веб-приложение и realtime

Для тестовой мойки создайте станцию с `connection_type: "mock"` и
`address: "mock://local"`. В админке появится **Управлять mock**: доступность,
баланс поста, завершение сессии, начисление текущему клиенту и очередь доставки.
Новый mock-пост онлайн с нулевым балансом; автоматической симуляции нет.

Администратор также может вызвать `POST /admin/stations/{id}/mock`:
`{"request_id":"manual-balance-0001","action":"set_state","balance":200}`.
Действия: `set_state` (`online` и/или `balance`), `finish` (освобождение и обнуление),
`accrue` (`diff > 0` текущему клиенту). Каждый новый запрос имеет уникальный
`request_id`, повтор после потери ответа — тот же ID и параметры.
`GET /admin/stations/{id}/mock/events` показывает доставку начислений;
`POST /admin/stations/{id}/mock/events/{event_id}/retry` с `{}` повторяет событие
после исправления причины ошибки. Все эти методы требуют admin JWT или ADMIN_TOKEN.
`accrue` подтверждает очередь, фактическое начисление подтверждает статус `delivered`.

Фронтенд находится в соседнем `washing_loalty_front`; запуск — в его README.
Новая миграция `002_frontend.sql` добавляет учёт списаний и разрешает отрицательные
изменения в истории. Прежняя миграция не изменяется.

| Метод | Назначение |
| --- | --- |
| `GET /me` | Проверенные `role`, `expires_at`, `user` и `pending_redemption` для пользователя |
| `POST /admin/login` | Обмен `Authorization: Bearer ADMIN_TOKEN` на admin JWT и cookie |
| `POST /logout` | Удаление сессионной cookie |
| `PUT /admin/stations/{id}` | Замена редактируемых полей станции; те же поля и лимиты, что у создания |
| `GET /admin/users?page=1&q=alice` | Поиск и статистика пользователей: `balance`, `earned`, `spent`, `operations` |
| `GET /admin/statistics` | Общие `users`, `stations`, `balance`, `earned`, `spent`; суммы передаются строками |
| `POST /redeem` | `qr`, положительный `amount`, уникальный `request_id` длиной 16–64 ASCII букв/цифр/`_-` |
| `WS /ws/station` | Подключение к станции или подписка на поток состояния |

Административные методы принимают admin JWT в cookie/Bearer; прежний статический
`ADMIN_TOKEN` остаётся доступен через Bearer для интеграций. JWT администратора
отзывается при смене `ADMIN_TOKEN`. `role` отсутствующего старого пользовательского
токена считается `user`. JWT проверяется сервером; decode на клиенте не даёт прав.
`GET /me` для admin не возвращает личный кошелёк: служебная сессия отдельная.

Cookie: `HttpOnly`, `SameSite=Lax`, `Path=/`, срок совпадает с токеном.
Установите `COOKIE_SECURE=true` при TLS на внешнем reverse proxy. При прямом TLS
флаг Secure ставится автоматически. Изменяющие cookie-запросы проверяют Origin;
WS принимает Origin того же Host. Клиенты вне браузера могут использовать Bearer.
Reverse proxy обязан сохранять публичный Host, включая порт.

### WebSocket

Первый JSON кадр — одна команда (до 2 KiB, в течение 15 секунд):

```json
{"type":"connect","qr":"station-qr","action":"save"}
```

или:

```json
{"type":"connect","qr":"station-qr","action":"spend","amount":25,"request_id":"550e8400-e29b-41d4-a716-446655440000"}
```

Успех: `{"type":"connected","qr":"station-qr"}`, затем сокет команды закрывается.
Для потока открывается новый сокет с `{"type":"subscribe","qr":"station-qr"}`.
Backend получает WS-события от коннектора и отдаёт `type: "state"`, `station`,
текущий бонусный `balance` и `state: {online, linked_to_user, balance}`.
`state.balance` включён только для владельца привязки; чужой ID не выдаётся.
Отсутствие поддержки WS у коннектора возвращает ошибку, HTTP polling не используется.

Ошибка: `{"type":"error","error":{"code":"station_busy","status":409}}`.
Есть ping/pong, таймауты, ограничение размера кадра и повторная проверка JWT при
событиях/каждые 10 секунд. Сброс пароля закрывает действующую подписку; истечение
JWT закрывает её по таймеру. Browser reconnect создаёт только подписку, не списание.

### Надёжность списания

`redemption` сохраняет request_id, пользователя, сумму и снимок станции до вызова
коннектора. Пока результат `pending`, второе списание пользователя и изменение
этой станции запрещены. При подтверждении отрицательная история и уменьшение
баланса выполняются атомарно один раз. На гарантированный `409` коннектора запись
переходит в `rejected`, бонусы не списываются. Повтор с тем же ID возвращает тот же
исход; изменение пользователя, QR или суммы отклоняется.

Таймаут, обрыв или некорректное подтверждение неоднозначны: запись остаётся pending.
Её нужно повторить с тем же request_id (фронтенд показывает кнопку проверки даже
после перезагрузки через `/me`). Нельзя удалять резерв или создавать новый ID,
пока оборудование могло применить перевод. Коннектор обязан сохранять результаты
идемпотентно, включая отказы, и не применять перевод дважды. Автоматического
фонового обработчика здесь нет; восстановление инициируется пользователем.

Префикс `redemption:` у `event_id` зарезервирован для списаний и отклоняется
в `/internal/accruals`. Оборудование не должно начислять бонусы за перевод бонусов
на станцию. Курс бонусов к физическим единицам задаётся коннектором.
