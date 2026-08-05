import argparse
import json
import os
import random
import sys
import time
from contextlib import contextmanager
from dataclasses import asdict
from dataclasses import dataclass
from dataclasses import field
from datetime import datetime
from enum import StrEnum
from pathlib import Path

from google_auth_oauthlib.flow import InstalledAppFlow
from googleapiclient.discovery import build
from googleapiclient.errors import HttpError


def youtube_authenticate_oauth(filename):
    flow = InstalledAppFlow.from_client_secrets_file(Path.cwd() / filename, scopes=['https://www.googleapis.com/auth/youtube'])
    return build('youtube', 'v3', credentials=flow.run_local_server())


def get_user_confirmation(message):
    print(message)
    while True:
        answer = input('Continue? ').strip().lower()
        match answer:
            case 'yes' | 'y':
                return
            case 'no' | 'n':
                print('Exiting')
                sys.exit(0)
            case _:
                print('Choose "yes" or "no"')


def chunks(lst, n):
    """Yield chunks of size n from a list."""
    for i in range(0, len(lst), n):
        yield lst[i : i + n]


def split_evenly(lst, target_size):
    """Split close to the target_size.

    If number remaining is greater than half the target_size,
    add an extra list and distribute the remaining elements.

    Args:
        lst: List to split
        target_size: Target size of each list

    Returns:
        list[list[Any]]: List of lists split as evenly as possible
    """
    length = len(lst)
    num_lists, remain = divmod(length, target_size)
    if remain > length // 2:
        num_lists += 1
    split_size, remain = divmod(length, num_lists)
    new_lists = []
    start = 0
    for i in range(num_lists):
        end = start + split_size
        if i < remain:
            end += 1  # add extra element until all remaining are gone
        new_lists.append(lst[start:end])
        start = end
    return new_lists


def time_to_words(seconds):
    hours = seconds // 3600
    minutes = (seconds % 3600) // 60
    seconds = round(seconds % 60)
    if seconds >= 60:
        seconds = 0.0
        minutes += 1
    if minutes >= 60:
        minutes = 0.0
        hours += 1
    return f'{hours} hours, {minutes} minutes, {seconds} seconds'


def view_logs(filename):
    if Path(filename).exists():
        with open(filename) as f:
            data = json.load(f)
            for log in data['progress_logs']:
                print(log)
    else:
        print(f'File {filename} does not exist.')


def view_stats(filename):
    if Path(filename).exists():
        with open(filename) as f:
            data = json.load(f)
            for playlist in data['playlists']:
                print(playlist['title'].ljust(20), len(playlist['videos']))
    else:
        print(f'File {filename} does not exist.')


def view_video_errors(filename):
    splitter = YoutubePlaylistSplitter(checkpoint_filename=filename)
    splitter.load()
    for playlist in splitter.data.playlists:
        for video in playlist.videos:
            if video.status == VideoStatus.ERROR:
                print(f'{playlist.title} | {video.title} | {video.error_message}')


class QuotaExceededError(Exception):
    pass


class VideoStatus(StrEnum):
    PENDING = 'pending'
    SUCCESS = 'success'
    ERROR = 'error'


@dataclass
class PlaylistVideo:
    id: str
    id_with_playlist: str | None = None
    previous_id_with_playlist: str | None = None
    title: str | None = None
    playlist_id: str | None = None
    previous_playlist_id: str | None = None
    status: VideoStatus = VideoStatus.PENDING
    error_message: str = ''


@dataclass
class Playlist:
    id: str | None
    title: str
    description: str
    videos: list[PlaylistVideo]


@dataclass
class PlaylistSplitterData:
    last_run_time: float = 0
    quota_exceeded: bool = False
    playlists: list[Playlist] = field(default_factory=list)
    progress_logs: list[str] = field(default_factory=list)


class YoutubePlaylistSplitter:
    ONE_DAY = 60 * 60 * 24
    FOUR_HOURS = 60 * 60 * 4

    def __init__(self, checkpoint_filename: str):
        self.checkpoint_filename = checkpoint_filename
        self.playlist_id_cache: dict[str, str] = {}

    def load(self) -> None:
        """
        Load progress data from file into `self.data`.

        If the file does not exist, create empty `self.data` and save it.
        """

        if not Path(self.checkpoint_filename).exists():
            self.data = PlaylistSplitterData()
            self.log_info('Created new progress data file.')
            self.save()
            return
        with open(self.checkpoint_filename) as f:
            data = json.load(f)
            data['playlists'] = [Playlist(**playlist) for playlist in data['playlists']]
            for playlist in data['playlists']:
                playlist.videos = [PlaylistVideo(**video) for video in playlist.videos]
            self.data = PlaylistSplitterData(**data)
            self.log_info('Loaded progress data file.')

    def save(self) -> None:
        """
        Save `self.data` to `self.checkpoint_filename` as json.

        Mark the timestamp before saving.
        """
        self._mark_timestamp()
        self.log_info('Saving...')
        with open(self.checkpoint_filename, 'w') as f:
            json.dump(asdict(self.data), f, indent=4)

    def _mark_timestamp(self):
        """
        Mark `self.data.last_run_time` with the current time in epoch seconds.
        """
        self.data.last_run_time = time.time()

    def _log(self, message: str):
        """
        Create log message and append it to `self.data.progress_logs`.

        Return the log message for printing to console.
        Do Not save in this method, as some log messages are
        console only (e.g. errors) and should not be saved.
        """
        timestamp = datetime.now().strftime('%Y-%m-%d %H:%M:%S')
        level, message = message.split(' | ', 1)
        output = f'{level} {timestamp} | {message}'
        self.data.progress_logs.append(output)
        return output

    def log_error(self, message):
        output = f'[ERROR] | {message}'
        print(self._log(output))

    def log_info(self, message):
        output = f'[INFO]  | {message}'
        print(self._log(output))

    def authenticate(self, filename):
        """
        Authenticate with YouTube API using OAuth2 credentials.
        """
        try:
            self.youtube = youtube_authenticate_oauth(filename)
        except Exception as e:
            self.log_error(f'Error authenticating with OAuth: {e}')
            sys.exit(1)

    @property
    def all_videos(self):
        return [video for playlist in self.data.playlists for video in playlist.videos]

    @contextmanager
    def handle_quota_exceeded(self):
        """
        Context manager to handle `HttpError` from the youtube API due to quota exceeded.

        Handle this error separately as the script must pause for 24 hours before continuing,
        but is not in an error state, so should not exit the script.
        However, the script can be safely exited while waiting. It will check the time remaining upon next start.

        Check error message and if quota is exceeded:
        - log error
        - save progress
        - set `self.data.quota_exceeded` to True
        - raise `QuotaExceededError`

        Raises:
            QuotaExceededError: If the quota is exceeded.
            HttpError: Original error from the API call if not quota exceeded.

        Example::

            request = self.youtube.playlists().insert(part='snippet,status', body=body)
            with self.handle_quota_exceeded():
                response = request.execute()
            return response['id']
        """
        try:
            yield
        except HttpError as e:
            error_message = json.loads(e.content.decode()).get('error', {}).get('message', '')
            if 'The request cannot be completed because you have exceeded' in error_message:
                self.log_error(error_message)
                raise QuotaExceededError from e
            else:
                raise e

    def check_for_quota_violation(self):
        """Check for API quota violation and wait 24 hours if exceeded.

        The script can be safely exited while waiting. It will check the time remaining upon next start.
        """
        self.log_info('Checking for quota violation...')
        if self.data.quota_exceeded:
            self.log_info('24 Hour Quota exceeded.')
            if self.data.last_run_time + self.ONE_DAY > time.time():
                additional_sleep = self.data.last_run_time + self.ONE_DAY - time.time() + 60  # add 60 seconds buffer
                self.log_info(f'{time_to_words(additional_sleep)} until quota reset.')
                recheck_time = min(self.FOUR_HOURS, additional_sleep)
                self.log_info(f'Re-check status in {time_to_words(recheck_time)}')
                time.sleep(recheck_time)
                self.check_for_quota_violation()
            else:
                self.log_info('24 hours have passed. Resetting quota violation.')
                self.data.quota_exceeded = False
        else:
            self.log_info('Quota not exceeded.')

    def has_playlists(self) -> bool:
        """
        Check if there is any data in `self.data.playlists`.
        """
        return bool(self.data.playlists)

    def all_videos_processed(self) -> bool:
        """
        Check if there are playlists (`self.data.playlists` is not empty) and no videos in any playlists with PENDING status.
        """
        if all(video.status != VideoStatus.PENDING for video in self.all_videos):
            self.log_info('All current videos processed. Congratulations!')
            self.save()
            return True
        return False

    def has_pending_videos_to_process(self) -> bool:
        """Any video in any playlist has status PENDING."""
        return any(video.status == VideoStatus.PENDING for video in self.all_videos)

    def process_pending_videos(self):
        """Process videos with status PENDING.

        Check each playlist for videos with PENDING status.
        If the playlist has no ID, it has not been created yet, so create it and assign that id to all videos in the playlist.

        :class:`QuotaExceededError` breaks out to be caught in the main while loop with :meth:`self.check_for_quota_violation`.

        :class:`HttpError` indicates an error with the video.  Do not mind the reason, log the error
        and set the video status to ERROR and move past.
        These errors may be video being deleted or set to private or being moved/renamed etc...

        :Note: Actions are split between those that require API calls and those that do not, in case
        of quota exceeded in the middle of processing.
        No changes are saved in `self.data` unless the API call was successful, or they do not require an API call.

        """
        for playlist in self.data.playlists:
            if playlist.id is None:
                new_playlist_id = self.create_playlist(title=playlist.title)
                self.log_info(f'Created playlist {playlist.title} with ID: {new_playlist_id}')
                playlist.id = new_playlist_id
                self.save()

            for video in playlist.videos:
                if video.status == VideoStatus.PENDING:
                    try:
                        id_with_playlist = self.add_video_to_playlist(playlist.id, video.id)
                        video.previous_id_with_playlist = video.id_with_playlist
                        video.id_with_playlist = id_with_playlist
                        video.previous_playlist_id = video.playlist_id
                        video.playlist_id = playlist.id
                        video.status = VideoStatus.SUCCESS
                        self.log_info(f'Video {video.title}: {video.id} added to playlist {playlist.title}: {playlist.id}')
                        self.save()
                    except QuotaExceededError:
                        raise
                    except HttpError as e:
                        error_message = json.loads(e.content.decode()).get('error', {}).get('message', '')
                        video.status = VideoStatus.ERROR
                        video.error_message = error_message
                        self.log_error(error_message)
                        self.save()

    def get_playlist_id_from_name(self, playlist_name: str) -> str:
        """Retrieve the playlist ID if the playlist can be found by name.

        Cached per instance: the main loop asks on every pass, and each miss is
        a quota-costing `playlists.list` call.
        """
        if playlist_name in self.playlist_id_cache:
            return self.playlist_id_cache[playlist_name]
        self.log_info(f"Searching for: '{playlist_name}'")
        request = self.youtube.playlists().list(part='snippet', mine=True, maxResults=50)
        with self.handle_quota_exceeded():
            response = request.execute()
        for item in response['items']:
            if item['snippet']['title'] == playlist_name:
                self.log_info(f'Found ID: {item["id"]}')
                self.playlist_id_cache[playlist_name] = item['id']
                return item['id']
        raise ValueError(f"Playlist '{playlist_name}' not found.")

    def get_videos_from_playlist_id(self, playlist_id) -> list[PlaylistVideo]:
        """Retrieve all videos from a playlist by playlist ID."""
        next_page_token = None
        videos = []
        while True:
            request = self.youtube.playlistItems().list(part='id,snippet', playlistId=playlist_id, maxResults=50, pageToken=next_page_token)
            with self.handle_quota_exceeded():
                response = request.execute()
            for item in response['items']:
                video_id = item['snippet']['resourceId']['videoId']
                video_title = item['snippet']['title']
                videos.append(PlaylistVideo(id=video_id, id_with_playlist=item['id'], title=video_title, playlist_id=playlist_id))
            if not (next_page_token := response.get('nextPageToken')):
                break
        self.log_info(f'Found {len(videos)} videos in playlist {playlist_id}')
        return videos

    def split_playlist_videos(self, videos, new_playlist_name, target_size):
        """Split videos into new shuffled playlists of target size.

        Playlists will be named with the new_playlist_name and a number suffix.
        Eg. new_playlist_name-1, new_playlist_name-2, etc...
        """
        random.shuffle(videos)
        for i, chunk in enumerate(split_evenly(videos, target_size=target_size), start=1):
            title = f'{new_playlist_name}-{i}'
            playlist = Playlist(id=None, title=title, description='', videos=[])
            for video in chunk:
                playlist.videos.append(video)
            self.data.playlists.append(playlist)
            self.log_info(f'Added {len(chunk)} videos to playlist {title}')
            self.save()
        self.log_info('All videos added to new playlists.')

    def create_playlist(self, title, description=''):
        body = {
            'snippet': {'title': title, 'description': description, 'defaultLanguage': 'en'},
            'status': {'privacyStatus': 'public'},
        }
        request = self.youtube.playlists().insert(part='snippet,status', body=body)
        with self.handle_quota_exceeded():
            response = request.execute()
        return response['id']

    def add_video_to_playlist(self, playlist_id, video_id) -> str:
        resource_id = {'kind': 'youtube#video', 'videoId': video_id}
        body = {'snippet': {'playlistId': playlist_id, 'resourceId': resource_id}}
        request = self.youtube.playlistItems().insert(part='snippet', body=body)
        with self.handle_quota_exceeded():
            response = request.execute()
        return response['id']

    def delete_playlist_videos(self, playlist_name, playlist_id):
        deleted_video_count = 0
        for video in self.all_videos:
            if (video.previous_playlist_id == playlist_id) and video.status in (VideoStatus.SUCCESS, VideoStatus.ERROR):
                request = self.youtube.playlistItems().delete(id=video.previous_id_with_playlist)
                try:  # Handle quota exceeded error inside otherwise HttpError will be caught first and not raise QuotaExceededError
                    with self.handle_quota_exceeded():
                        request.execute()
                        self.log_info(f'Deleted video {video.title} from playlist {playlist_name}: {playlist_id}')
                        video.previous_playlist_id = None
                        video.previous_id_with_playlist = None
                        self.save()
                        deleted_video_count += 1
                except HttpError as e:
                    error_message = json.loads(e.content.decode()).get('error', {}).get('message', '')
                    video.status = VideoStatus.ERROR
                    video.error_message = error_message
                    self.log_error(error_message)
                    self.save()
                finally:
                    self.log_info(f'Deleted {deleted_video_count} videos from playlist {playlist_name}')
                    self.save()

    def playlist_has_new_videos(self, playlist_id: str) -> list[PlaylistVideo]:
        """Check if there are videos in the playlist that are not in the data file."""
        self.log_info('Checking for new videos...')
        videos = self.get_videos_from_playlist_id(playlist_id)
        current_video_ids = [v.id for v in self.all_videos]
        if new_videos := [video for video in videos if video.id not in current_video_ids]:
            self.log_info(f'Found {len(new_videos)} new videos.')
        return new_videos

    def add_new_playlist_videos(self, new_videos: list[PlaylistVideo]):
        """Add new videos to existing playlists as PENDING status.

        Note: This should only be used if the playlist has been split and new playlists exist.
        This method adds new videos to existing playlists in a round robin fashion.
        """
        for video in new_videos:
            playlist_lengths = [len(playlist.videos) for playlist in self.data.playlists]
            max_playlist_length = max(playlist_lengths)
            if all([length == max_playlist_length for length in playlist_lengths]):
                first_playlist = self.data.playlists[0]
                first_playlist.videos.append(video)
                self.log_info(f'Added video {video.title} to playlist {first_playlist.title}')
            else:
                for playlist in self.data.playlists:
                    if len(playlist.videos) < max_playlist_length:
                        playlist.videos.append(video)
                        self.log_info(f'Added video {video.title} to playlist {playlist.title}')
                        break
        self.save()
        self.log_info(f'Added {len(new_videos)} new videos to playlists.')


def main(args):
    if args.view_logs:
        view_logs(args.checkpoint_file)
        sys.exit(0)

    if args.view_stats:
        view_stats(args.checkpoint_file)
        sys.exit(0)

    if args.view_video_errors:
        view_video_errors(args.checkpoint_file)
        sys.exit(0)

    playlist = args.playlist
    splitter = YoutubePlaylistSplitter(checkpoint_filename=args.checkpoint_file)
    splitter.load()
    splitter.authenticate(args.secret_file)

    while True:
        try:
            splitter.check_for_quota_violation()
            playlist_id = splitter.get_playlist_id_from_name(playlist)

            if splitter.has_pending_videos_to_process():
                splitter.process_pending_videos()

            if new_videos := splitter.playlist_has_new_videos(playlist_id):
                splitter.add_new_playlist_videos(new_videos)

            if args.delete_original:
                splitter.delete_playlist_videos(playlist, playlist_id)

            if not splitter.has_playlists():
                videos = splitter.get_videos_from_playlist_id(playlist_id)
                splitter.split_playlist_videos(videos, new_playlist_name=args.new_playlist, target_size=args.target_size)

            if splitter.all_videos_processed():
                sys.exit(0)

        except QuotaExceededError:
            splitter.data.quota_exceeded = True
            splitter.save()

        except KeyboardInterrupt:
            splitter.log_info('User interrupted. Exiting.')
            sys.exit(0)


if __name__ == '__main__':
    CHECKPOINT_FILE = 'split_playlist_progress.json'
    YOUTUBE_CLIENT_SECRET_FILENAME = 'client_secret.json'  # nosec
    PLAYLIST_TO_SPLIT = 'WSC'
    NEW_PLAYLIST_NAME = 'WSC'
    PLAYLIST_SIZE = 90

    os.environ['OAUTHLIB_INSECURE_TRANSPORT'] = '1'

    help_msg = """Split a YouTube playlist into smaller playlists of a target size.
    \n
    Example usage:
    python split_youtube_playlist.py --playlist "WSC" --target-size 90 --secret-file "client_secret.json"
    """

    parser = argparse.ArgumentParser(description=help_msg, formatter_class=argparse.RawTextHelpFormatter)
    parser.add_argument('--checkpoint-file', required=False, default=CHECKPOINT_FILE, help='Filename to load and save progress')
    parser.add_argument('--secret-file', required=False, default=YOUTUBE_CLIENT_SECRET_FILENAME, help='YouTube client secret filename')
    parser.add_argument('--playlist', required=False, default=PLAYLIST_TO_SPLIT, help='Name of playlist to split')
    parser.add_argument('--new-playlist', required=False, default=PLAYLIST_TO_SPLIT, help='Name of new playlists to create from split')
    parser.add_argument('--target-size', required=False, default=PLAYLIST_SIZE, help='Target size of the split playlists')
    parser.add_argument('--delete-original', required=False, action='store_true', help='Delete original playlist videos after splitting')
    parser.add_argument('--view-logs', required=False, action='store_true', help='View progress logs')
    parser.add_argument('--view-stats', required=False, action='store_true', help='View saved playlist stats')
    parser.add_argument('--view-video-errors', required=False, action='store_true', help='View videos with errors')
    args = parser.parse_args()

    main(args)
