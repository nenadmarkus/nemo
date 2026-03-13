const std = @import("std");

fn postJson(
    allocator: std.mem.Allocator,
    url: []const u8,
    api_key: []const u8,
    payload: anytype,
) !struct {
    status: std.http.Status,
    body: []u8,
} {
    const stringified = try std.json.Stringify.valueAlloc(allocator, payload, .{});
    defer allocator.free(stringified);

    const auth = try std.fmt.allocPrint(allocator, "Bearer {s}", .{api_key});
    defer allocator.free(auth);

    var client: std.http.Client = .{ .allocator = allocator };
    defer client.deinit();

    var aw: std.io.Writer.Allocating = .init(allocator);
    defer aw.deinit();

    const result = try client.fetch(.{
        .location = .{ .url = url },
        .method = .POST,
        .headers = .{
            .content_type = .{ .override = "application/json" },
            .authorization = .{ .override = auth },
        },
        .payload = stringified,
        .response_writer = &aw.writer,
    });

    const body = try allocator.dupe(u8, aw.written());

    return .{
        .status = result.status,
        .body = body,
    };
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

    const resp = try postJson(allocator, endpoint, api_key, request);
    defer allocator.free(resp.body);

    if (resp.status != .ok) {
        std.debug.print("HTTP {}: {s}\n", .{ resp.status, resp.body });
        return error.HttpError;
    }

    const parsed = std.json.parseFromSlice(std.json.Value, allocator, resp.body, .{}) catch {
        return try allocator.dupe(u8, resp.body);
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

    return try allocator.dupe(u8, resp.body);
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
