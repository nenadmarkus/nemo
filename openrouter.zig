const std = @import("std");

fn postJson(
    allocator: std.mem.Allocator,
    client: *std.http.Client,
    url: []const u8,
    api_key: []const u8,
    payload: anytype,
) ![]u8 {
    const stringified = try std.json.Stringify.valueAlloc(allocator, payload, .{});
    defer allocator.free(stringified);

    var auth_buf: [256]u8 = undefined; // fixed length buffer, no alloc
    if (api_key.len > auth_buf.len - 8) {
        return error.ApiKeyTooLong;
    }
    const auth = try std.fmt.bufPrint(&auth_buf, "Bearer {s}", .{api_key});

    var writer: std.io.Writer.Allocating = .init(allocator);

    const result = try client.fetch(.{
        .location = .{ .url = url },
        .method = .POST,
        .headers = .{
            .content_type = .{ .override = "application/json" },
            .authorization = .{ .override = auth },
        },
        .payload = stringified,
        .response_writer = &writer.writer,
    });

    const body = try writer.toOwnedSlice();
    errdefer allocator.free(body);

    if (result.status != .ok) {
        std.debug.print("HTTP {d}: {s}\n", .{ result.status, body });
        return error.HttpError;
    }

    return body;
}

fn invokeIntelligence(
    allocator: std.mem.Allocator,
    endpoint: []const u8,
    model: []const u8,
    api_key: []const u8,
    messages: anytype,
) ![]u8 {
    const request = .{
        .model = model,
        .messages = messages,
        .reasoning = .{ .enabled = false },
    };

    var client: std.http.Client = .{ .allocator = allocator };
    defer client.deinit();

    const resp = try postJson(allocator, &client, endpoint, api_key, request);
    defer allocator.free(resp);

    const parsed = std.json.parseFromSlice(std.json.Value, allocator, resp, .{}) catch |err| {
        std.debug.print("JSON parse error: {}\n", .{err});
        return try allocator.dupe(u8, resp);
    };
    defer parsed.deinit();

    if (parsed.value.object.get("choices")) |choices_val| {
        if (choices_val.array.items.len > 0) {
            const first_choice = choices_val.array.items[0];
            if (first_choice.object.get("message")) |msg| {
                if (msg.object.get("content")) |content| {
                    if (content == .string) {
                        return try allocator.dupe(u8, content.string);
                    }
                }
            }
        }
    }

    return try allocator.dupe(u8, resp);
}

pub fn main() !void {
    var gpa: std.heap.GeneralPurposeAllocator(.{}) = .{};
    defer _ = gpa.deinit();
    const allocator = gpa.allocator();

    const api_key = std.posix.getenv("OPENROUTER_API_KEY") orelse {
        std.debug.print("OPENROUTER_API_KEY not set\n", .{});
        return;
    };

    const messages = .{
        .{ .role = "system", .content = "You are an assistant." },
        .{ .role = "user", .content = "How many r's are in the word strawberry?" },
    };

    const response = try invokeIntelligence(allocator, "https://openrouter.ai/api/v1/chat/completions", "nvidia/nemotron-3-super-120b-a12b:free", api_key, messages);
    defer allocator.free(response);

    std.debug.print("{s}\n", .{response});
}
