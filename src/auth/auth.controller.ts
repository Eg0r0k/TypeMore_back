import {
  Controller,
  Post,
  Body,
  Req,
  Res,
  Logger,
  UseGuards
} from '@nestjs/common';
import { AuthService } from './auth.service';
import { Request, Response, response } from 'express';
import { RegisterUserDto, LoginUserDto } from './dto';
import { AuthGuard } from '@nestjs/passport';
import { AtGuards, RtGuards } from '../common/guards';
interface AuthenticatedUser {
  sub: string;
  refreshToken: string;
}
@Controller('auth')
export class AuthController {
  private readonly logger = new Logger(AuthController.name);
  constructor(private readonly authService: AuthService) {}
  @Post('login')
  async login(
    @Req() request: Request,
    @Res() response: Response,
    @Body() loginDto: LoginUserDto
  ): Promise<any> {
    try {
      const maxAgeCookieToken = 24 * 60 * 60 * 1000;
      const result = await this.authService.login(loginDto);
      response.cookie('jwt', result, {
        httpOnly: true,
        maxAge: maxAgeCookieToken,
        secure: process.env.NODE_ENV === 'production'
      });
      return response.status(200).json({
        status: 'OK',
        message: 'Successfully login!',
        result: result
      });
    } catch (error) {
      return response.status(500).json({
        status: 'Error',
        message: 'Server Error!'
      });
    }
  }

  @UseGuards(AuthGuard('jwt'))
  @Post('logout')
  async logout(
    @Req() request: Request & { user: AuthenticatedUser },
    @Res() response: Response
  ) {
    try {
      const userId = request.user.sub;
      const result = await this.authService.logout(userId);
      this.logger.log('User successfully logout');
      return response.status(200).json({
        status: 'OK',
        message: 'Successfully logout',
        result: result
      });
    } catch (error: any) {
      this.logger.error(`Logout error: ${error.message}`, error.stack);
      return response.status(500).json({
        status: 'Error',
        message: error.message
      });
    }
  }

  // @UseGuards(AuthGuard('jwt-refresh'))
  // @Post('refresh')
  // async refreshTokens(
  //   @Req() request: Request & { user: AuthenticatedUser },
  //   @Res() response: Response
  // ) {
  //   try {
  //     const user = request.user;
  //     const result = await this.authService.refreshTokens(
  //       user['sub'],
  //       user['refreshToken']
  //     );
  //     this.logger.log('Refresh token generated succsessfully');
  //     return response.status(200).json({
  //       status: 'OK',
  //       message: 'Successfully generated',
  //       result: result
  //     });
  //   } catch (error: any) {
  //     this.logger.error(`Refresh token error: ${error.message}`, error.stack);
  //   }
  // }

  @Post('/register')
  async register(
    @Req() request: Request,
    @Res() response: Response,
    @Body() registerDto: RegisterUserDto
  ): Promise<any> {
    try {
      const result = await this.authService.register(registerDto);
      this.logger.log(`User successfully registred`);
      return response.status(200).json({
        status: 'OK',
        message: 'Successfully register user!',
        result: result
      });
    } catch (error: any) {
      this.logger.error(`Registration error: ${error.message}`, error.stack);
      return response.status(500).json({
        status: 'Error',
        message: error.message
      });
    }
  }
}
