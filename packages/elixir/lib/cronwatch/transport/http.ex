defmodule Cronwatch.Transport.HTTP do
  @moduledoc """
  The default `Cronwatch.Transport`: one HTTP/1.1 POST over OTP's own
  `:gen_tcp` and `:ssl`, so the package needs no HTTP client of its own.

  It does only what the channels and triage need, and does it the one way,
  hardened:

    * One request per connection (`connection: close`), nothing kept
      between sends, no proxy read from the environment (Node's fetch reads
      none).
    * A redirect is never followed: its 3xx is the answer, and the channel
      fails on it.
    * The answer's head is held to 64 KiB and 256 lines; its body is handed
      to the channel's request a chunk at a time as it arrives, whatever
      the status and however it is framed (a `content-length`, chunked, or
      up to the connection's close), so the request stops reading at 1 MiB.
    * No `accept-encoding` is sent and nothing is decompressed, so a
      compressed answer cannot grow in memory.
    * TLS is always verified: `verify: :verify_peer` against the system's
      roots (`:public_key.cacerts_get/0`), the host checked as HTTPS checks
      it. A system without them (a slim or distroless image) fails each
      https request with an error that says so and names `cacerts:`, found
      once and remembered. A host name is sent as SNI and checked against the certificate;
      an IP address is never sent as SNI (the protocol has no place for
      one) and the certificate is checked against the address, because
      `server_name_indication: :disable` would turn `:ssl`'s hostname check
      off, so it is never given.
    * The host is the one the channel's URL was read to, connected to as
      read, so what the URL names and what is reached cannot differ.

  `Post` holds the whole request to its deadline, killing the process that
  runs it, and the socket goes with that process. The answer's `body` and
  `close` keep the connection's state in the dictionary of the process
  that called `post/2`, so they are called in that process, as
  `Cronwatch.Transport` asks: a `body` called from any other process answers
  `{:error, :other_process}`, and a `close` from one closes the socket. A
  transport of the app's own that wraps this one reads the body where it
  called it. `timeout:` (30 seconds by
  default) is a backstop for a transport called outside `Post`.

  Options: `cacerts:` (DER certificates to trust in place of the system's,
  for a private certificate authority), `ssl:` (further `:ssl` client
  options, added after these), and `timeout:`.

  The request carries `host`, then the headers given, in their order, then
  `content-length` (unless given), and `connection: close`, and no
  `user-agent` unless given one.
  """

  @behaviour Cronwatch.Transport

  alias Cronwatch.Alerts.URL
  alias Cronwatch.Transport.Request
  alias Cronwatch.Transport.Response

  @backstop 30_000
  # The most an answer's head may take, and how many lines it may have.
  @max_head 65_536
  @max_lines 256
  # The longest chunk-size line read before the chunk is refused.
  @max_chunk_line 4096
  # Interim (1xx) answers skipped before the real one.
  @max_interim 8

  @impl true
  def post(opts, %Request{} = r) do
    opts = if is_list(opts), do: opts, else: []
    deadline = System.monotonic_time(:millisecond) + Keyword.get(opts, :timeout, @backstop)

    with {:ok, u} <- target(r.url),
         {:ok, sock} <- connect(u, opts, deadline),
         :ok <- send_request(sock, request(u, r)),
         {:ok, status, headers, buf} <- read_head(sock, "", deadline, 0) do
      framing = framing(status, headers)
      state = %{sock: sock, buf: buf, mode: framing, deadline: deadline}

      case framing do
        {:error, reason} ->
          close(sock)
          {:error, reason}

        _ ->
          # The connection's state is kept in this process's dictionary, so
          # the body is read here, as Cronwatch.Transport asks; called from
          # another process it fails rather than answer an empty body, and
          # a close from there closes the socket all the same.
          key = {__MODULE__, make_ref()}
          owner = self()
          Process.put(key, state)

          {:ok,
           %Response{
             status: status,
             body: fn -> if self() == owner, do: next(key), else: {:error, :other_process} end,
             close: fn -> if self() == owner, do: finish(key), else: close(sock) end
           }}
      end
    end
  end

  defp target(url) do
    case URL.parse(url) do
      {:ok, %URL{scheme: s} = u} when s in ["http", "https"] -> {:ok, u}
      _ -> {:error, :invalid_url}
    end
  end

  defp left(deadline), do: max(0, deadline - System.monotonic_time(:millisecond))

  # Headers that frame the request or name its connection: this transport
  # writes its own, and one given is left out, as fetch leaves out the
  # forbidden ones, so no header given can change how the body is read.
  @own ~w(host content-length transfer-encoding connection keep-alive te trailer upgrade expect)

  # The request's bytes: the request line, host, the headers as given,
  # content-length, and connection: close.
  defp request(%URL{} = u, %Request{headers: headers, body: body}) do
    target = if u.query, do: u.path <> "?" <> u.query, else: u.path
    host = u.host <> if(u.port, do: ":#{u.port}", else: "")
    given = for {n, v} <- headers, String.downcase(n) not in @own, do: {n, v}
    all = [{"host", host}] ++ given ++ [{"content-length", Integer.to_string(byte_size(body))}, {"connection", "close"}]
    lines = for {n, v} <- all, do: [n, ": ", v, "\r\n"]
    ["POST ", target, " HTTP/1.1\r\n", lines, "\r\n", body]
  end

  # The least time one address is given to connect, when there are several
  # to share what is left of the deadline.
  @min_attempt 2_000

  # Connecting: every address the host has, IPv4 first, until one answers.
  defp connect(%URL{} = u, opts, deadline) do
    port = u.port || if(u.scheme == "https", do: 443, else: 80)

    with {:ok, opts} <- with_cacerts(u, opts),
         {:ok, addresses} <- addresses(u.host, deadline) do
      try_addresses(addresses, u, port, opts, deadline, :nxdomain)
    end
  end

  # The roots an https request is verified against, found before anything
  # is resolved or connected, so a system without any fails at once.
  defp with_cacerts(%URL{scheme: "https"}, opts) do
    with {:ok, certs} <- cacerts(opts), do: {:ok, Keyword.put(opts, :cacerts, certs)}
  end

  defp with_cacerts(_u, opts), do: {:ok, opts}

  @doc false
  # The roots to trust: those given as `cacerts:`, else the system's.
  def cacerts(opts) do
    case Keyword.get(opts, :cacerts) do
      nil -> system_cacerts()
      certs -> {:ok, certs}
    end
  end

  # :public_key keeps the system's roots once it has read them; a system
  # with none (a slim or distroless image) is remembered too, so each
  # request after the first fails at once without reading the disk again.
  defp system_cacerts do
    case :persistent_term.get({__MODULE__, :no_cacerts}, nil) do
      nil ->
        case read_system_cacerts(&:public_key.cacerts_get/0) do
          {:ok, certs} ->
            {:ok, certs}

          {:error, message} ->
            :persistent_term.put({__MODULE__, :no_cacerts}, message)
            {:error, message}
        end

      message ->
        {:error, message}
    end
  end

  @doc false
  # The system's roots as `loader` reads them, or an error naming the
  # option that does without them.
  def read_system_cacerts(loader) do
    case loader.() do
      [_ | _] = certs -> {:ok, certs}
      _ -> {:error, no_cacerts("it has none")}
    end
  catch
    _kind, reason -> {:error, no_cacerts(inspect(reason))}
  end

  defp no_cacerts(why) do
    "the system's trusted root certificates could not be read (#{why}); install them " <>
      "(the ca-certificates package) or pass cacerts: to Cronwatch.Transport.HTTP"
  end

  defp addresses("[" <> v6, _deadline) do
    case :inet.parse_ipv6strict_address(String.to_charlist(String.trim_trailing(v6, "]"))) do
      {:ok, a} -> {:ok, [a]}
      _ -> {:error, :invalid_url}
    end
  end

  defp addresses(host, deadline) do
    name = String.to_charlist(host)

    case :inet.parse_ipv4strict_address(name) do
      {:ok, a} ->
        {:ok, [a]}

      _ ->
        # Both families at once, neither past the deadline: a resolver that
        # hangs on one costs only the time that was left.
        lookups = for family <- [:inet, :inet6], do: Task.async(fn -> getaddrs(name, family) end)

        found =
          Enum.flat_map(lookups, fn task ->
            case Task.yield(task, left(deadline)) || Task.shutdown(task, :brutal_kill) do
              {:ok, list} -> list
              _ -> []
            end
          end)

        if found == [], do: {:error, :nxdomain}, else: {:ok, found}
    end
  end

  defp getaddrs(name, family) do
    case :inet.getaddrs(name, family) do
      {:ok, list} -> list
      _ -> []
    end
  end

  defp try_addresses([], _u, _port, _opts, _deadline, last), do: {:error, last}

  # Each address gets a share of what is left, so one that never answers
  # does not spend the whole deadline before the next is tried; the last
  # gets all that remains.
  defp try_addresses([address | rest] = all, u, port, opts, deadline, _last) do
    remaining = left(deadline)
    within = min(remaining, max(div(remaining, length(all)), @min_attempt))

    case open(address, u, port, opts, within) do
      {:ok, sock} -> {:ok, sock}
      # A certificate refused is the answer; another address would say the same.
      {:error, {:tls_alert, _} = reason} -> {:error, reason}
      {:error, reason} -> try_addresses(rest, u, port, opts, deadline, reason)
    end
  end

  @tcp [:binary, packet: :raw, active: false, nodelay: true]

  defp open(address, %URL{scheme: "http"}, port, _opts, within) do
    case :gen_tcp.connect(address, port, @tcp ++ family(address), within) do
      {:ok, s} -> {:ok, {:tcp, s}}
      e -> e
    end
  end

  defp open(address, %URL{scheme: "https"} = u, port, opts, within) do
    case :ssl.connect(address, port, @tcp ++ family(address) ++ ssl(u.host, opts), within) do
      {:ok, s} -> {:ok, {:ssl, s}}
      e -> e
    end
  end

  defp family(address) when tuple_size(address) == 8, do: [:inet6]
  defp family(_), do: [:inet]

  @doc false
  # The TLS options: always verified, against the system's roots (or the
  # ones given), the host checked as HTTPS checks it, a name sent as SNI.
  def ssl(host, opts) do
    ip? = String.starts_with?(host, "[") or match?({:ok, _}, :inet.parse_ipv4strict_address(String.to_charlist(host)))

    cacerts =
      case cacerts(opts) do
        {:ok, certs} -> certs
        {:error, message} -> raise ArgumentError, message
      end

    # An IP address is not sent as SNI, and :ssl checks the certificate
    # against the address it connected to. `server_name_indication:
    # :disable` would turn that check off too, so it is never given.
    sni = if ip?, do: [], else: [server_name_indication: String.to_charlist(host)]

    [verify: :verify_peer, cacerts: cacerts, depth: 10] ++
      sni ++
      [customize_hostname_check: [match_fun: :public_key.pkix_verify_hostname_match_fun(:https)]] ++
      Keyword.get(opts, :ssl, [])
  end

  # Writes the request; a write that fails closes the socket, as every
  # failure after connecting does.
  defp send_request(sock, data) do
    case write(sock, data) do
      :ok ->
        :ok

      e ->
        close(sock)
        e
    end
  end

  defp write({:tcp, s}, data), do: :gen_tcp.send(s, data)
  defp write({:ssl, s}, data), do: :ssl.send(s, data)

  defp recv({:tcp, s}, within), do: :gen_tcp.recv(s, 0, within)
  defp recv({:ssl, s}, within), do: :ssl.recv(s, 0, within)

  defp close({:tcp, s}), do: :gen_tcp.close(s)
  defp close({:ssl, s}), do: :ssl.close(s)

  # The answer's head: the status and the headers (names lowercased), and
  # what arrived after it. Interim 1xx answers are skipped.
  defp read_head(sock, buf, deadline, interim) do
    case :binary.match(buf, "\r\n\r\n") do
      {at, 4} ->
        head = binary_part(buf, 0, at)
        rest = binary_part(buf, at + 4, byte_size(buf) - at - 4)

        case parse_head(head) do
          {:ok, status, _} when status in 100..199 and status != 101 and interim < @max_interim ->
            read_head(sock, rest, deadline, interim + 1)

          {:ok, status, _} when status < 200 ->
            close(sock)
            {:error, :invalid_response}

          {:ok, status, headers} ->
            {:ok, status, headers, rest}

          e ->
            close(sock)
            e
        end

      :nomatch when byte_size(buf) > @max_head ->
        close(sock)
        {:error, :header_too_long}

      :nomatch ->
        case recv(sock, left(deadline)) do
          {:ok, data} ->
            read_head(sock, buf <> data, deadline, interim)

          {:error, :closed} ->
            close(sock)
            {:error, if(buf == "", do: :socket_closed_remotely, else: :invalid_response)}

          {:error, reason} ->
            close(sock)
            {:error, reason}
        end
    end
  end

  defp parse_head(head) when byte_size(head) > @max_head, do: {:error, :header_too_long}

  defp parse_head(head) do
    [line | lines] = :binary.split(head, "\r\n", [:global])

    if length(lines) > @max_lines do
      {:error, :header_too_long}
    else
      case status(line) do
        {:ok, status} -> {:ok, status, Enum.flat_map(lines, &header/1)}
        :error -> {:error, :invalid_response}
      end
    end
  end

  defp status(<<"HTTP/1.", v, " ", a, b, c, rest::binary>>)
       when v in [?0, ?1] and a in ?1..?9 and b in ?0..?9 and c in ?0..?9 and
              (rest == "" or binary_part(rest, 0, 1) == " ") do
    {:ok, (a - ?0) * 100 + (b - ?0) * 10 + (c - ?0)}
  end

  defp status(_), do: :error

  defp header(line) do
    case :binary.split(line, ":") do
      [name, value] -> [{String.downcase(name), trim(value)}]
      _ -> []
    end
  end

  defp trim(s), do: s |> String.trim_leading(" ") |> String.trim_leading("\t") |> String.trim_trailing()

  # How the body is framed: none, chunked, a length, or up to the close.
  defp framing(status, _headers) when status in [204, 304], do: {:length, 0}

  defp framing(_status, headers) do
    codings = for {"transfer-encoding", v} <- headers, c <- String.split(v, ","), do: c |> trim() |> String.downcase()
    lengths = for {"content-length", v} <- headers, do: v

    cond do
      codings != [] and List.last(codings) == "chunked" -> {:chunked, :size}
      codings != [] -> :close
      lengths == [] -> :close
      true -> content_length(lengths)
    end
  end

  # Every content-length given must say the same number.
  defp content_length(values) do
    values = values |> Enum.flat_map(&String.split(&1, ",")) |> Enum.map(&trim/1) |> Enum.uniq()

    case values do
      [digits] when byte_size(digits) in 1..15 ->
        if digits =~ ~r/\A[0-9]+\z/, do: {:length, String.to_integer(digits)}, else: {:error, :invalid_response}

      _ ->
        {:error, :invalid_response}
    end
  end

  # The next part of the body: {:ok, bytes}, :done or {:error, reason}.
  defp next(key) do
    case Process.get(key) do
      nil ->
        :done

      state ->
        case step(state) do
          {:ok, data, state} ->
            Process.put(key, state)
            {:ok, data}

          {:done, state} ->
            close(state.sock)
            Process.delete(key)
            :done

          {:error, reason, state} ->
            close(state.sock)
            Process.delete(key)
            {:error, reason}
        end
    end
  end

  defp finish(key) do
    case Process.delete(key) do
      nil -> :ok
      state -> close(state.sock)
    end
  end

  defp more(state) do
    case recv(state.sock, left(state.deadline)) do
      {:ok, data} -> {:ok, %{state | buf: state.buf <> data}}
      {:error, reason} -> {:error, reason, state}
    end
  end

  defp step(%{mode: {:length, 0}} = state), do: {:done, state}

  defp step(%{mode: {:length, n}, buf: buf} = state) when buf != "" do
    take = min(n, byte_size(buf))
    data = binary_part(buf, 0, take)
    {:ok, data, %{state | buf: binary_part(buf, take, byte_size(buf) - take), mode: {:length, n - take}}}
  end

  defp step(%{mode: {:length, _}} = state) do
    with {:ok, state} <- more(state), do: step(state)
  end

  defp step(%{mode: :close, buf: buf} = state) when buf != "", do: {:ok, buf, %{state | buf: ""}}

  defp step(%{mode: :close} = state) do
    case more(state) do
      {:ok, state} -> step(state)
      {:error, :closed, state} -> {:done, state}
      e -> e
    end
  end

  defp step(%{mode: {:chunked, :size}, buf: buf} = state) do
    case :binary.match(buf, "\r\n") do
      {at, 2} ->
        line = binary_part(buf, 0, at)
        rest = binary_part(buf, at + 2, byte_size(buf) - at - 2)

        case chunk_size(line) do
          {:ok, 0} -> {:done, state}
          {:ok, n} -> step(%{state | buf: rest, mode: {:chunked, {:data, n}}})
          :error -> {:error, :invalid_chunk, state}
        end

      :nomatch when byte_size(buf) > @max_chunk_line ->
        {:error, :invalid_chunk, state}

      :nomatch ->
        with {:ok, state} <- more(state), do: step(state)
    end
  end

  defp step(%{mode: {:chunked, {:data, n}}, buf: buf} = state) when buf != "" do
    take = min(n, byte_size(buf))
    data = binary_part(buf, 0, take)
    buf = binary_part(buf, take, byte_size(buf) - take)
    mode = if take == n, do: {:chunked, :crlf}, else: {:chunked, {:data, n - take}}
    {:ok, data, %{state | buf: buf, mode: mode}}
  end

  defp step(%{mode: {:chunked, :crlf}, buf: "\r\n" <> rest} = state),
    do: step(%{state | buf: rest, mode: {:chunked, :size}})

  defp step(%{mode: {:chunked, :crlf}, buf: buf} = state) when byte_size(buf) >= 2,
    do: {:error, :invalid_chunk, state}

  defp step(%{mode: {:chunked, _}} = state) do
    with {:ok, state} <- more(state), do: step(state)
  end

  # A chunk-size line: hex digits, then optional extensions after ";".
  defp chunk_size(line) do
    digits = line |> :binary.split(";") |> hd() |> String.trim_trailing(" ") |> String.trim_trailing("\t")

    if byte_size(digits) in 1..15 and digits =~ ~r/\A[0-9A-Fa-f]+\z/,
      do: {:ok, String.to_integer(digits, 16)},
      else: :error
  end
end
